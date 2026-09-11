package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// A failing orchestrator start is a banner on every tab — before this it was
// only in the daemon log.
func TestOrchestratorFailureBanner(t *testing.T) {
	m := Model{width: 200, height: 30}
	m.data.daemonHealth = frontend.DaemonHealth{Running: true, OrchestratorFailing: true,
		OrchestratorError: "starting it failed: agent_pane_busy"}
	if view := m.View(); !strings.Contains(view, "orchestrator could not start") || !strings.Contains(view, "agent_pane_busy") {
		t.Errorf("a failing orchestrator must show a banner, got:\n%s", view)
	}
}

// The Config tab checks the orchestrator's working directory the way the
// daemon does and warns beside the setting — ahead of the value, so a narrow
// terminal cannot truncate it away. The default and an existing directory get
// no warning (the controls).
func TestConfigTabWarnsOfAMissingOrchestratorCwd(t *testing.T) {
	cfg := config.Default()
	key := frontend.FSPOrchestratorCwdFieldKey
	if w := configFieldWarning(cfg, key); w != "" {
		t.Fatalf("the default (created on demand) was flagged: %q", w)
	}
	cfg.FullSelfPrompting.OrchestratorAgentCwd = t.TempDir()
	if w := configFieldWarning(cfg, key); w != "" {
		t.Fatalf("an existing directory was flagged: %q", w)
	}
	missing := filepath.Join(t.TempDir(), "gone")
	cfg.FullSelfPrompting.OrchestratorAgentCwd = missing
	label := configFieldLabel(cfg, key)
	if !strings.Contains(label, "does not exist") || strings.Index(label, "⚠") > strings.LastIndex(label, missing) {
		t.Fatalf("label = %q, want the warning ahead of the value", label)
	}

	m := Model{width: 90, height: 30}
	m.data.cfg = cfg
	m.items = buildRuleItems(cfg)
	var warned bool
	for _, ln := range m.configLines() {
		if ln.warn {
			warned = true
			if !strings.Contains(ln.text, "⚠") {
				t.Errorf("a warn row lost its marker to truncation: %q", ln.text)
			}
		}
	}
	if !warned {
		t.Fatal("no Config tab row was marked as a warning")
	}

	if err := os.Mkdir(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	if w := configFieldWarning(cfg, key); w != "" {
		t.Fatalf("still flagged after the directory was created: %q", w)
	}
}
