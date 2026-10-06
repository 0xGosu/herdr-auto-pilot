package tui

import (
	"strings"
	"testing"
	"time"
)

// TestAgentsTabHidesAgentsOfANodeOfflineOverADay: a machine silent for longer
// than domain.NodeOfflineHideAfter drops off the Agents tab, the separator says
// how many were hidden, and a machine that is merely stale stays listed.
func TestAgentsTabHidesAgentsOfANodeOfflineOverADay(t *testing.T) {
	m := fleetModel(t, 1, 3, 40)
	now := time.Now()
	st := &m.data.status
	st.RemoteAgents[0].LastHeard = now.Add(-25 * time.Hour) // gone a day: hidden
	st.RemoteAgents[1].LastHeard = now.Add(-2 * time.Hour)  // stale: listed
	// RemoteAgents[2] has no last-heard time at all: unknown, so listed.

	rows := m.visibleAgents()
	var names []string
	var sep string
	for _, r := range rows {
		if r.sep {
			sep = r.Name
			continue
		}
		names = append(names, r.Name)
	}
	got := strings.Join(names, ",")
	if strings.Contains(got, "away-a") {
		t.Errorf("an agent of a node offline 25h is listed: %s", got)
	}
	if !strings.Contains(got, "away-b") || !strings.Contains(got, "away-c") {
		t.Errorf("a stale or never-timed node's agent was hidden: %s", got)
	}
	if !strings.Contains(sep, "2 agents; 1 hidden") {
		t.Errorf("separator = %q, want it to count the hidden agent", sep)
	}
}

// TestAgentsTabKeepsTheSeparatorWhenEveryRemoteAgentIsHidden: with every other
// node offline the separator is the only trace, so it must still be drawn —
// otherwise the operator concludes those machines' agents are gone — and the
// cursor must not land on it as a real agent.
func TestAgentsTabKeepsTheSeparatorWhenEveryRemoteAgentIsHidden(t *testing.T) {
	m := fleetModel(t, 1, 2, 40)
	old := time.Now().Add(-3 * 24 * time.Hour)
	for i := range m.data.status.RemoteAgents {
		m.data.status.RemoteAgents[i].LastHeard = old
	}
	rows := m.visibleAgents()
	if len(rows) != 2 || !rows[1].sep || !strings.Contains(rows[1].Name, "0 agents; 2 hidden") {
		t.Fatalf("rows = %+v, want the local agent then a separator counting 2 hidden", rows)
	}
	m.cursors[tabAgents] = 1
	if r := m.selectedAgentRow(); r != nil {
		t.Errorf("the separator was selectable as an agent: %+v", r)
	}
	if !strings.Contains(m.View(), "2 hidden: node offline > 24h") {
		t.Errorf("the view does not say agents were hidden:\n%s", m.View())
	}
}
