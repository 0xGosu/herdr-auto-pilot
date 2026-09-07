package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// Remote agents used to be rendered in a dimmed block below the list that the
// cursor could never reach, on the last page only, and that the search filter
// never saw. Nothing in this package constructed a frontend.RemoteAgent at all,
// which is why two bugs lived there unnoticed. These tests cover that block.

// fleetModel builds an Agents tab holding `local` agents on this machine and
// `remote` on a node labelled "laptop".
func fleetModel(t *testing.T, local, remote int, height int) Model {
	t.Helper()
	m := Model{width: 120, height: height}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.NodeID = "aaaaaaaaaaaaaaaa"
	msg.status.SelfLabel = "here"
	msg.status.AgentNames = map[string]string{}
	msg.status.AgentStats = map[string]domain.AgentStats{}
	msg.status.Workspaces = map[string]domain.WorkspaceInfo{"w1": {ID: "w1", Number: 1}}
	msg.status.Tabs = map[string]domain.TabInfo{"t1": {ID: "t1", WorkspaceID: "w1", Label: "2"}}
	for i := 0; i < local; i++ {
		id := "local-p" + string(rune('a'+i))
		msg.status.MonitoredAgents = append(msg.status.MonitoredAgents, domain.AgentTransition{
			AgentID: id, PaneID: id, TabID: "t1", WorkspaceID: "w1",
			AgentType: "claude", Status: "idle",
		})
		msg.status.AgentNames[id] = "here-" + string(rune('a'+i))
	}
	for i := 0; i < remote; i++ {
		id := "remote-p" + string(rune('a'+i))
		msg.status.RemoteAgents = append(msg.status.RemoteAgents, frontend.RemoteAgent{
			RosterAgent: domain.RosterAgent{
				NodeID: "bbbbbbbbbbbbbbbb", AgentID: id, PaneID: id,
				TabID: "rt1", WorkspaceID: "rw1", AgentType: "codex", Status: "idle",
			},
			Name: "away-" + string(rune('a'+i)), NodeLabel: "laptop",
		})
	}
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabAgents
	return m
}

func agentRowLines(t *testing.T, m Model) []string {
	t.Helper()
	var out []string
	for _, ln := range strings.Split(m.View(), "\n") {
		if strings.Contains(ln, "here-") || strings.Contains(ln, "away-") {
			out = append(out, ln)
		}
	}
	return out
}

// The point of the whole change: a local agent's LOCATION is its herdr
// workspace/tab position, a remote one's is the NODE. A remote machine's
// #<ws>-<tab> means nothing on the machine you are looking at, while the node
// is the one thing you need in order to act on the row.
func TestLocationColumnCarriesTheNodeForRemoteRows(t *testing.T) {
	m := fleetModel(t, 1, 1, 30)
	rows := m.visibleAgents()
	var local, remote *agentRow
	for i := range rows {
		switch {
		case rows[i].sep:
		case rows[i].remote():
			remote = &rows[i]
		default:
			local = &rows[i]
		}
	}
	if local == nil || remote == nil {
		t.Fatalf("want one local and one remote row, got %+v", rows)
	}
	if local.Location != "#1-2" {
		t.Errorf("local LOCATION = %q, want the herdr position #1-2", local.Location)
	}
	if remote.Location != "laptop" {
		t.Errorf("remote LOCATION = %q, want the node label", remote.Location)
	}
	// And NAME is the bare short name: the node lives in LOCATION now, so
	// repeating it would spend a third of the name column saying it twice.
	if remote.Name != "away-a" {
		t.Errorf("remote NAME = %q, want the bare short name (no @node)", remote.Name)
	}
	for _, ln := range agentRowLines(t, m) {
		if strings.Contains(ln, "away-a@") {
			t.Errorf("remote row still renders name@node:\n%s", ln)
		}
	}
}

// The cursor must reach a remote row. Before this it structurally could not:
// visibleAgents returned only MonitoredAgents, so rowCountFor clamped the
// cursor short of every remote row and each per-agent key silently acted on
// the local selection instead.
func TestCursorReachesRemoteRows(t *testing.T) {
	m := fleetModel(t, 2, 2, 40)
	rows := m.visibleAgents()
	// 2 local + separator + 2 remote.
	if len(rows) != 5 {
		t.Fatalf("visibleAgents() = %d rows, want 5 (2 local, a separator, 2 remote): %+v", len(rows), rows)
	}
	if got := m.rowCountFor(tabAgents); got != 5 {
		t.Errorf("rowCountFor(tabAgents) = %d, want 5 — the cursor cannot reach what it does not count", got)
	}
	m.cursors[tabAgents] = 4
	r := m.selectedAgentRow()
	if r == nil || !r.remote() || r.Name != "away-b" {
		t.Fatalf("cursor at the last row selected %+v, want the second remote agent", r)
	}
}

// The separator is a row so the paging window stays exact, but it is not an
// agent: every per-agent key must no-op on it rather than act on a neighbour.
func TestTheSeparatorRowSelectsNoAgent(t *testing.T) {
	m := fleetModel(t, 1, 1, 30)
	rows := m.visibleAgents()
	sepAt := -1
	for i, r := range rows {
		if r.sep {
			sepAt = i
		}
	}
	if sepAt < 0 {
		t.Fatalf("no separator row: %+v", rows)
	}
	m.cursors[tabAgents] = sepAt
	if r := m.selectedAgentRow(); r != nil {
		t.Errorf("the separator selected agent %+v, want none", r)
	}
}

// The filter now runs over remote rows. It did not before, in both directions:
// a query showed the entire remote list unfiltered, and a query matching no
// LOCAL agent hid the remote rows entirely by falling into the empty branch.
func TestSearchFilterAppliesToRemoteRows(t *testing.T) {
	m := fleetModel(t, 2, 2, 40)
	m.setQuery(tabAgents, "away-a")
	rows := m.visibleAgents()
	if len(rows) != 2 || !rows[0].sep || rows[1].Name != "away-a" {
		t.Fatalf("filter 'away-a' = %+v, want the separator and one remote row", rows)
	}
}

// A filter matching only remote agents must still show them. This is the
// regression: with zero local matches the renderer took the "no agents" branch
// and the remote block vanished.
func TestAFilterMatchingOnlyRemoteAgentsStillShowsThem(t *testing.T) {
	m := fleetModel(t, 2, 1, 40)
	m.setQuery(tabAgents, "laptop") // the node label is searchable
	rows := m.visibleAgents()
	if len(rows) != 2 || rows[1].Name != "away-a" {
		t.Fatalf("filter 'laptop' = %+v, want the separator and the remote row", rows)
	}
	view := m.View()
	if !strings.Contains(view, "away-a") {
		t.Errorf("a filter matching only remote agents rendered no rows:\n%s", view)
	}
	if strings.Contains(view, "no agents") {
		t.Errorf("rendered the empty-list message while a remote row matched:\n%s", view)
	}
}

// The old block rendered on the last page only, so a remote agent past the
// first page was unreachable. As an ordinary row it pages like any other.
func TestRemoteRowsPageLikeAnyOther(t *testing.T) {
	// A short window forces more than one page.
	m := fleetModel(t, 6, 3, 14)
	total := len(m.visibleAgents())
	if total != 10 {
		t.Fatalf("want 10 rows (6 local, separator, 3 remote), got %d", total)
	}
	page := m.listPageSize()
	if page >= total {
		t.Fatalf("page size %d does not force paging over %d rows", page, total)
	}
	// Walk to the very last row; it must be the last remote agent, on screen.
	m.cursors[tabAgents] = total - 1
	m.scrollCursorIntoView()
	r := m.selectedAgentRow()
	if r == nil || r.Name != "away-c" {
		t.Fatalf("last row = %+v, want the last remote agent", r)
	}
	start, end := m.window(total)
	if m.cursors[tabAgents] < start || m.cursors[tabAgents] >= end {
		t.Errorf("cursor %d outside the visible window [%d,%d) — paging does not account for the separator",
			m.cursors[tabAgents], start, end)
	}
	if !strings.Contains(m.View(), "away-c") {
		t.Errorf("the last remote agent is not on screen:\n%s", m.View())
	}
}

// The stale marker used to be truncated to 8 characters, which erased the word
// for every status longer than two. It must be visible AND must not widen the
// STATUS column, which agentsRowFmt fixes at ten.
func TestTheStaleMarkerIsVisibleAndDoesNotShiftTheColumns(t *testing.T) {
	m := fleetModel(t, 1, 1, 30)
	m.data.status.RemoteAgents[0].Stale = true
	m.data.status.RemoteAgents[0].Disabled = true

	rows := m.visibleAgents()
	var remote agentRow
	for _, r := range rows {
		if r.remote() {
			remote = r
		}
	}
	got := agentRowStatus(remote)
	if !strings.Contains(got, "*") {
		t.Errorf("status = %q, want a visible stale marker", got)
	}
	if len(got) > 10 {
		t.Errorf("status = %q (%d chars); agentsRowFmt gives STATUS 10, so a longer value "+
			"shifts every column after it", got, len(got))
	}
	// And the columns still line up with the header.
	var header, row string
	for _, ln := range strings.Split(m.View(), "\n") {
		switch {
		case header == "" && strings.Contains(ln, "NAME"):
			header = ln
		case strings.Contains(ln, "away-a"):
			row = ln
		}
	}
	if header == "" || row == "" {
		t.Fatalf("header/row not rendered:\n%s", m.View())
	}
	if got, want := len(strings.Fields(row)), len(strings.Fields(header)); got != want {
		t.Errorf("remote row has %d fields, header has %d:\n%s\n%s", got, want, header, row)
	}
}

// THE ID-COLLISION REGRESSION.
//
// Status.AgentName, StatsFor and AgentDisabled are keyed by agent id alone, and
// an agent id IS a herdr pane id — which repeats on every machine sharing the
// store. Read through those maps a remote row would show a LOCAL agent's name,
// counters and disabled flag, and `x` would disable the wrong agent on the
// wrong machine. Every display value is resolved at row-build time instead.
func TestARemoteRowSharingALocalPaneIDShowsItsOwnState(t *testing.T) {
	m := Model{width: 120, height: 30}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.NodeID = "aaaaaaaaaaaaaaaa"
	// Both machines have a pane "1" — the ordinary case, not an exotic one.
	msg.status.MonitoredAgents = []domain.AgentTransition{
		{AgentID: "1", PaneID: "1", AgentType: "claude", Status: "idle"},
	}
	msg.status.AgentNames = map[string]string{"1": "mine"}
	msg.status.DisabledAgents = map[string]bool{"1": true}
	msg.status.AgentStats = map[string]domain.AgentStats{"1": {Escalations: 99}}
	msg.status.RemoteAgents = []frontend.RemoteAgent{{
		RosterAgent: domain.RosterAgent{
			NodeID: "bbbbbbbbbbbbbbbb", AgentID: "1", PaneID: "1",
			AgentType: "codex", Status: "working",
		},
		Name: "theirs", NodeLabel: "laptop", Disabled: false,
		Stats: domain.AgentStats{Escalations: 3},
	}}
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabAgents

	var remote agentRow
	for _, r := range m.visibleAgents() {
		if r.remote() {
			remote = r
		}
	}
	if remote.Name != "theirs" {
		t.Errorf("remote NAME = %q, want theirs — it read the local agent's name off an id-keyed map", remote.Name)
	}
	if remote.Disabled {
		t.Error("remote row reads as DISABLED; that is the LOCAL agent's flag under the same pane id")
	}
	if remote.Stats.Escalations != 3 {
		t.Errorf("remote escalations = %d, want 3 — it read the local agent's counters", remote.Stats.Escalations)
	}
	if remote.NodeID != "bbbbbbbbbbbbbbbb" {
		t.Errorf("remote NodeID = %q; without it every action targets the wrong machine", remote.NodeID)
	}
}

// A remote agent whose node has stopped reporting is refused BEFORE anything is
// queued, and the message names the machine. requireLiveDaemonFor says the same
// thing, but only after the operator has waited out a round trip that was never
// going to happen.
func TestAStaleRemoteRowRefusesEveryActionUpFront(t *testing.T) {
	m := fleetModel(t, 0, 1, 30)
	m.data.status.RemoteAgents[0].Stale = true
	m.cursors[tabAgents] = 1 // past the separator, onto the remote row

	for _, tc := range []struct {
		key string
		run func(Model) (Model, string)
	}{
		{"n", func(m Model) (Model, string) { u, _ := m.renameSelected(); return u.(Model), u.(Model).message }},
		{"x", func(m Model) (Model, string) {
			u, _ := m.disableSelectedAgentPrompt()
			return u.(Model), u.(Model).message
		}},
		{"f", func(m Model) (Model, string) { u, _ := m.focusSelected(); return u.(Model), u.(Model).message }},
	} {
		got, msg := tc.run(m)
		if !strings.Contains(msg, "laptop") || !strings.Contains(msg, "stopped reporting") {
			t.Errorf("%q on a stale remote row said %q, want a refusal naming the node", tc.key, msg)
		}
		if got.prompt != nil || got.confirm != nil {
			t.Errorf("%q on a stale remote row opened a prompt/confirmation anyway", tc.key)
		}
	}
}

// Focusing a remote agent moves THAT machine's herdr. The operator presses a
// key and their own screen does not change, so without the sentence the only
// reasonable reading is that nothing worked.
func TestRemoteFocusSaysWhoseViewMoved(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	m := fleetModel(t, 0, 1, 30)
	m.app = &frontend.App{Store: st, ConfigPath: seedLocalFSConfigIn(t, dir), Author: "operator"}
	m.ctx = context.Background()
	m.cursors[tabAgents] = 1

	upd, cmd := m.focusSelected()
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("focus produced no command")
	}
	// The banner text is decided before the command runs; the command itself
	// will fail here (no daemon), which is not what this asserts.
	msg, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("focus returned %T, want actionResultMsg", cmd())
	}
	if msg.err == nil && !strings.Contains(msg.message, "laptop") {
		t.Errorf("focus banner = %q, want it to name the node whose view moved", msg.message)
	}
	if msg.err == nil && !strings.Contains(msg.message, "has not moved") {
		t.Errorf("focus banner = %q, want it to say this machine's view did NOT move", msg.message)
	}
}

// The rename prompt and the disable confirmation both name the machine, so an
// operator cannot mistake a remote agent for the local one beside it.
func TestRemoteRenameAndDisablePromptsNameTheNode(t *testing.T) {
	m := fleetModel(t, 1, 1, 30)
	m.ctx = context.Background()
	m.cursors[tabAgents] = 2 // past the local row and the separator

	upd, _ := m.renameSelected()
	rm := upd.(Model)
	if rm.prompt == nil || !strings.Contains(rm.prompt.label, "on node laptop") {
		t.Errorf("rename prompt = %+v, want it to name node laptop", rm.prompt)
	}

	upd, _ = m.disableSelectedAgentPrompt()
	dm := upd.(Model)
	if dm.confirm == nil || !strings.Contains(dm.confirm.label, "on node laptop") {
		t.Errorf("disable confirmation = %+v, want it to name node laptop", dm.confirm)
	}
}

// The detail overlay must not render fields it cannot know for a remote agent.
// Workspaces/Tabs, AgentMode and AgentCwd are all keyed by an id in THIS node's
// namespace, so showing them would attribute a local stranger's workspace, mode
// and working directory to the remote agent.
func TestRemoteAgentDetailShowsTheNodeAndSuppressesLocalOnlyFields(t *testing.T) {
	m := fleetModel(t, 0, 1, 30)
	m.data.status.AgentModes = map[string]domain.AgentMode{"remote-pa": domain.AgentModePlan}
	m.data.status.AgentCwds = map[string]string{"remote-pa": "/local/checkout"}
	m.cursors[tabAgents] = 1

	var remote agentRow
	for _, r := range m.visibleAgents() {
		if r.remote() {
			remote = r
		}
	}
	detail := strings.Join(m.agentDetailLines(remote, m.width), "\n")
	if !strings.Contains(detail, "laptop") {
		t.Errorf("remote detail does not name its node:\n%s", detail)
	}
	for _, field := range []string{"Mode", "Working dir", "Workspace", "Tab", "Task source"} {
		if strings.Contains(detail, field) {
			t.Errorf("remote detail renders local-only field %q, which describes a pane on THIS machine:\n%s",
				field, detail)
		}
	}
}

// FillAgentModes reads a pane on THIS machine. A remote agent id is a herdr
// pane id that repeats across machines, so asking for its mode would drive a
// pane read against whichever local agent shares the id — every refresh tick.
func TestAnOpenRemoteDetailAsksForNoLocalPaneRead(t *testing.T) {
	m := fleetModel(t, 0, 1, 30)
	m.cursors[tabAgents] = 1
	upd, _ := m.viewSelected()
	m = upd.(Model)
	if m.detail == nil || m.detail.agent == nil {
		t.Fatal("no agent detail opened")
	}
	if got := m.detailAgentID(); got != "" {
		t.Errorf("detailAgentID() = %q while a REMOTE detail is open, want \"\" — "+
			"that id would drive a pane read on this machine", got)
	}
	// A local detail still asks, or the mode field goes blank for everyone.
	lm := fleetModel(t, 1, 1, 30)
	lm.cursors[tabAgents] = 0
	upd, _ = lm.viewSelected()
	lm = upd.(Model)
	if got := lm.detailAgentID(); got == "" {
		t.Error("a LOCAL agent detail must still request its mode read")
	}
}

// "See tasks" for a remote agent goes through the fleet task lists, never
// through this node's [[task_sources]] — config never enters the database, so
// this machine has no entry describing another machine's source.
func TestSeeTasksOnARemoteAgentJumpsToItsFleetList(t *testing.T) {
	m := fleetModel(t, 0, 1, 40)
	m.data.fleetTasks = []frontend.TaskGroup{{
		Source:    config.TaskSource{Agent: "away-a"},
		Index:     -1,
		Locator:   "db://bbbbbbbbbbbbbbbb/away-a.md",
		Display:   "away-a.md (hap database, node bbbbbbbb)",
		NodeID:    "bbbbbbbbbbbbbbbb",
		NodeLabel: "laptop",
		Items:     []domain.ChecklistItem{{Text: "ship it"}},
	}}
	m.cursors[tabAgents] = 1

	upd, _ := m.showSelectedAgentTasks()
	m = upd.(Model)
	if m.tab != tabTasks {
		t.Fatalf("tab = %v, want the Tasks tab", m.tab)
	}
	r := m.selectedTaskRow()
	if r == nil || !r.header {
		t.Fatalf("selected row = %+v, want the remote group's header", r)
	}
	// Fleet groups are laid out after this node's own.
	if want := len(m.data.tasks); r.group != want {
		t.Errorf("landed on group %d, want %d (the first fleet group)", r.group, want)
	}
}

// A remote agent whose node keeps its list in a FILE is invisible from here by
// construction. The refusal has to say that, not "add a task source on the
// Config tab" — which names this machine's config, where there is nothing to
// fix.
func TestSeeTasksOnARemoteAgentWithoutASharedListSaysWhy(t *testing.T) {
	m := fleetModel(t, 0, 1, 40)
	m.cursors[tabAgents] = 1

	upd, _ := m.showSelectedAgentTasks()
	m = upd.(Model)
	if m.tab != tabAgents {
		t.Errorf("tab = %v, want to stay on Agents", m.tab)
	}
	if !strings.Contains(m.message, "laptop") ||
		!strings.Contains(m.message, string(config.ProviderSQLite)) {
		t.Errorf("message = %q, want it to name the node and the sqlite provider", m.message)
	}
	if strings.Contains(m.message, "Config tab") {
		t.Errorf("message = %q points at THIS machine's config, where there is nothing to fix", m.message)
	}
}

// With no remote agents at all — every single-node install — the list is
// exactly what it was: no separator, no behaviour change.
func TestASingleNodeListHasNoSeparator(t *testing.T) {
	m := fleetModel(t, 3, 0, 30)
	rows := m.visibleAgents()
	if len(rows) != 3 {
		t.Fatalf("visibleAgents() = %d rows, want 3", len(rows))
	}
	for _, r := range rows {
		if r.sep || r.remote() {
			t.Errorf("single-node list has a separator or remote row: %+v", r)
		}
	}
	if strings.Contains(m.View(), "other nodes") {
		t.Errorf("single-node view renders the fleet separator:\n%s", m.View())
	}
}
