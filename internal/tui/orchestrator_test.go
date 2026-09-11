package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The orchestrator's row is set apart from the herd: an operator must never
// mistake the session hap ignores for one of the agents it drives.
func TestOrchestratorRowIsHighlighted(t *testing.T) {
	if !isOrchestratorRow(agentRow{Name: domain.OrchestratorAgentName}) {
		t.Error("the orchestrator's row is not recognised")
	}
	if isOrchestratorRow(agentRow{Name: "otter"}) {
		t.Error("an ordinary agent was treated as the orchestrator")
	}
	if isOrchestratorRow(agentRow{Name: domain.OrchestratorAgentName, sep: true}) {
		t.Error("a separator row was treated as the orchestrator")
	}
	for name, p := range themes {
		st := newStyles(p)
		if !st.orchestrator.GetBold() || st.orchestrator.GetForeground() != lipgloss.TerminalColor(p.title) {
			t.Errorf("theme %s: the orchestrator style is not the bold title colour", name)
		}
	}

	m := Model{width: 140, height: 30}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.AgentNames = map[string]string{"wO:p1": domain.OrchestratorAgentName, "w1:p1": "otter"}
	msg.status.AgentStats = map[string]domain.AgentStats{}
	msg.status.MonitoredAgents = []domain.AgentTransition{
		{AgentID: "wO:p1", PaneID: "wO:p1", AgentType: "claude", Status: "idle"},
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	}
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabAgents
	if view := m.View(); !strings.Contains(view, domain.OrchestratorAgentName) || !strings.Contains(view, "otter") {
		t.Fatalf("the Agents tab lost a row:\n%s", view)
	}
}
