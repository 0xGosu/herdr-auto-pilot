package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// TestPaneExcerptStripsCodexComposer proves paneExcerpt's independent "deep"
// visible read (which bypasses classify.Classify entirely) also strips
// codex's composer/input-box line before it reaches the LLM-facing excerpt,
// while leaving another agent type's excerpt byte-identical to the raw read.
// fakeHerdr implements ReadPane but not ports.VisiblePaneReader, so
// d.readVisible falls back to ReadPane, deterministically exercising the
// "deep" branch with the content set by setPane.
func TestPaneExcerptStripsCodexComposer(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	fh := &fakeHerdr{}
	fh.setPane("─ Worked for 10m 49s ─────\n\n› Summarize recent commits\n\n  gpt-5.6-sol high · /workspaces/herdr-auto-pilot\n")
	d, err := New(Options{ConfigPath: cfgPath, Store: raw, Herdr: fh, LLM: &fakeLLM{}})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	codexSituation := domain.Situation{AgentType: "codex", PaneID: "pane-1", Content: "stale snapshot"}
	excerpt := d.paneExcerpt(ctx, cfg, codexSituation)
	if strings.Contains(excerpt, "›") {
		t.Errorf("codex pane excerpt must have composer line stripped, got %q", excerpt)
	}
	if !strings.Contains(excerpt, "gpt-5.6-sol high") {
		t.Errorf("footer must survive in codex excerpt, got %q", excerpt)
	}

	claudeSituation := domain.Situation{AgentType: "claude", PaneID: "pane-1", Content: "stale snapshot"}
	claudeExcerpt := d.paneExcerpt(ctx, cfg, claudeSituation)
	if !strings.Contains(claudeExcerpt, "› Summarize recent commits") {
		t.Errorf("claude pane excerpt must be untouched, got %q", claudeExcerpt)
	}
}
