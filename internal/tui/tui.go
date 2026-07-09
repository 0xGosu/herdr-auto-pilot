// Package tui is the primary control surface, run as a Herdr pane. It
// mirrors every CLI capability (FR-022): monitored agents (with rename),
// pending escalations (confirm/correct), the audit log (post-hoc
// correction), rules and thresholds (view AND edit: config fields,
// allowlist patterns, task sources, clear-data), and the pause/kill switch
// with history.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

type tab int

const (
	tabAgents tab = iota
	tabEscalations
	tabAudit
	tabRules
	tabKill
	tabCount
)

var tabNames = []string{"Agents", "Escalations", "Audit", "Rules", "Pause/Kill"}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	activeTab     = lipgloss.NewStyle().Bold(true).Underline(true)
	inactiveTab   = lipgloss.NewStyle().Faint(true)
	pausedStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	runningStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("46"))
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	sectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
	helpStyle     = lipgloss.NewStyle().Faint(true)
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
)

type refreshMsg struct {
	status      frontend.Status
	escalations []domain.AuditRecord
	audit       []domain.AuditRecord
	kills       []domain.KillEvent
	cfg         config.Config
	err         error
}

type actionResultMsg struct {
	message string
	err     error
}

type tickMsg time.Time

// prompt is an in-flight inline input.
type prompt struct {
	label    string
	input    string
	onSubmit func(string) tea.Cmd
}

// ruleItem is one navigable row of the Rules tab.
type ruleItem struct {
	kind  string // "field" | "pattern" | "source"
	key   string // config field key (fields)
	index int    // slice index (patterns / sources)
	value string // pattern text / source path — verified on removal
	label string // rendered row
}

// Model is the Bubble Tea model.
type Model struct {
	app *frontend.App
	ctx context.Context

	tab     tab
	cursor  int
	data    refreshMsg
	items   []ruleItem // Rules tab rows, rebuilt on refresh
	message string
	prompt  *prompt
	width   int
	height  int
}

// New creates the TUI model.
func New(ctx context.Context, app *frontend.App) Model {
	return Model{app: app, ctx: ctx}
}

// Init starts the refresh loop.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) refresh() tea.Cmd {
	app, ctx := m.app, m.ctx
	return func() tea.Msg { return refreshData(ctx, app) }
}

func refreshData(ctx context.Context, app *frontend.App) refreshMsg {
	var msg refreshMsg
	msg.status, msg.err = app.GetStatus(ctx)
	if msg.err != nil {
		return msg
	}
	msg.escalations, msg.err = app.Escalations(ctx)
	if msg.err != nil {
		return msg
	}
	msg.audit, msg.err = app.Audit(ctx, 50)
	if msg.err != nil {
		return msg
	}
	msg.kills, msg.err = app.KillHistory(ctx, 50)
	if msg.err != nil {
		return msg
	}
	msg.cfg, msg.err = app.Config()
	return msg
}

// buildRuleItems lays out the Rules tab rows from the current config.
func buildRuleItems(cfg config.Config) []ruleItem {
	var items []ruleItem
	for _, key := range frontend.ConfigFieldKeys {
		items = append(items, ruleItem{
			kind: "field", key: key,
			label: fmt.Sprintf("%-38s %s", key, frontend.FieldValue(cfg, key)),
		})
	}
	for i, p := range cfg.Safety.AllowlistPatterns {
		items = append(items, ruleItem{
			kind: "pattern", index: i, value: p,
			label: fmt.Sprintf("allowlist #%d  %s", i, p),
		})
	}
	for i, src := range cfg.TaskSources {
		sel := src.Agent
		if sel == "" {
			sel = "*"
		}
		ws := src.Workspace
		if ws == "" {
			ws = "*"
		}
		items = append(items, ruleItem{
			kind: "source", index: i, value: src.Path,
			label: fmt.Sprintf("task-source #%d  agent=%s ws=%s  %s", i, sel, ws, src.Path),
		})
	}
	return items
}

// Update handles events.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case refreshMsg:
		m.data = msg
		m.items = buildRuleItems(msg.cfg)
		if m.cursor >= m.rowCount() {
			m.cursor = max(0, m.rowCount()-1)
		}
		return m, nil
	case actionResultMsg:
		if msg.err != nil {
			m.message = errStyle.Render(msg.err.Error())
		} else if msg.message != "" {
			m.message = okStyle.Render(msg.message)
		}
		return m, m.refresh()
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.prompt != nil {
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEnter:
			p := m.prompt
			m.prompt = nil
			input := strings.TrimSpace(p.input)
			if input == "" {
				m.message = "cancelled"
				return m, nil
			}
			return m, p.onSubmit(input)
		case tea.KeyEsc:
			m.prompt = nil
			m.message = "cancelled"
			return m, nil
		case tea.KeyBackspace:
			if r := []rune(m.prompt.input); len(r) > 0 {
				m.prompt.input = string(r[:len(r)-1])
			}
			return m, nil
		case tea.KeySpace:
			m.prompt.input += " "
			return m, nil
		case tea.KeyRunes:
			// Only printable input; key names like "up"/"home" must not
			// leak into the text.
			m.prompt.input += string(msg.Runes)
			return m, nil
		default:
			return m, nil
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "right", "l":
		m.tab = (m.tab + 1) % tabCount
		m.cursor = 0
		m.message = ""
	case "shift+tab", "left", "h":
		m.tab = (m.tab + tabCount - 1) % tabCount
		m.cursor = 0
		m.message = ""
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		m.message = ""
	case "down", "j":
		if m.cursor < m.rowCount()-1 {
			m.cursor++
		}
		m.message = ""
	case "p":
		return m, m.do("automation paused", func(ctx context.Context) error { return m.app.Pause(ctx) })
	case "r":
		return m, m.do("automation resumed", func(ctx context.Context) error { return m.app.Resume(ctx) })
	case "enter", "y":
		switch m.tab {
		case tabEscalations:
			return m.confirmSelected()
		case tabRules:
			return m.editSelectedRule()
		}
	case "e":
		if m.tab == tabRules {
			return m.editSelectedRule()
		}
	case "c":
		if m.tab == tabEscalations || m.tab == tabAudit {
			return m.correctSelected()
		}
	case "n":
		if m.tab == tabAgents {
			return m.renameSelected()
		}
	case "a":
		if m.tab == tabRules {
			return m.addPatternPrompt()
		}
	case "t":
		if m.tab == tabRules {
			return m.addTaskSourcePrompt()
		}
	case "x", "delete":
		if m.tab == tabRules {
			return m.removeSelectedRule()
		}
	case "X":
		if m.tab == tabRules {
			return m.clearDataPrompt()
		}
	}
	return m, nil
}

// do runs a mutation and reports its outcome.
func (m Model) do(okMsg string, fn func(context.Context) error) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		if err := fn(ctx); err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{message: okMsg}
	}
}

func (m Model) rowCount() int {
	switch m.tab {
	case tabAgents:
		return len(m.data.status.MonitoredAgents)
	case tabEscalations:
		return len(m.data.escalations)
	case tabAudit:
		return len(m.data.audit)
	case tabRules:
		return len(m.items)
	case tabKill:
		return len(m.data.kills)
	}
	return 0
}

func (m Model) selectedAudit() *domain.AuditRecord {
	switch m.tab {
	case tabEscalations:
		if m.cursor < len(m.data.escalations) {
			return &m.data.escalations[m.cursor]
		}
	case tabAudit:
		if m.cursor < len(m.data.audit) {
			return &m.data.audit[m.cursor]
		}
	}
	return nil
}

// --- Escalation / audit actions ---

func (m Model) confirmSelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	id := rec.ID
	return m, m.do(fmt.Sprintf("confirmed #%d and sent", id), func(ctx context.Context) error {
		return m.app.Confirm(ctx, id, true)
	})
}

func (m Model) correctSelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	id := rec.ID
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: fmt.Sprintf("correct #%d — action to record", id),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if err := app.Resolve(ctx, id, input, false); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("correction recorded for #%d", id)}
			}
		},
	}
	return m, nil
}

// --- Agent rename ---

func (m Model) renameSelected() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.data.status.MonitoredAgents) {
		return m, nil
	}
	agent := m.data.status.MonitoredAgents[m.cursor]
	current := m.data.status.AgentName(agent.AgentID)
	target := agent.AgentID
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: fmt.Sprintf("rename %s (%s) to", orDash(current), agent.AgentID),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if err := app.RenameAgent(ctx, target, input); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("agent renamed to %q", input)}
			}
		},
	}
	return m, nil
}

// --- Rules tab editing ---

func (m Model) selectedRule() *ruleItem {
	if m.tab == tabRules && m.cursor < len(m.items) {
		return &m.items[m.cursor]
	}
	return nil
}

func (m Model) editSelectedRule() (tea.Model, tea.Cmd) {
	item := m.selectedRule()
	if item == nil || item.kind != "field" {
		return m, nil
	}
	key := item.key
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: fmt.Sprintf("set %s (current %s)", key, frontend.FieldValue(m.data.cfg, key)),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if err := app.SetField(ctx, key, input); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: key + " updated (daemon reloaded)"}
			}
		},
	}
	return m, nil
}

func (m Model) addPatternPrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: "add never-auto allowlist regex",
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if err := app.AddAllowlistPattern(ctx, input); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: "allowlist pattern added"}
			}
		},
	}
	return m, nil
}

func (m Model) addTaskSourcePrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: "add task source: <path> [agent] [workspace]",
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				parts := strings.Fields(input)
				if len(parts) > 3 {
					return actionResultMsg{err: fmt.Errorf(
						"expected <path> [agent] [workspace] — got %d fields (paths with spaces are not supported here; use the CLI)", len(parts))}
				}
				var agent, workspace string
				if len(parts) > 1 {
					agent = parts[1]
				}
				if len(parts) > 2 {
					workspace = parts[2]
				}
				if err := app.AddTaskSource(ctx, agent, workspace, parts[0]); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: "task source added"}
			}
		},
	}
	return m, nil
}

func (m Model) removeSelectedRule() (tea.Model, tea.Cmd) {
	item := m.selectedRule()
	if item == nil {
		return m, nil
	}
	app := m.app
	switch item.kind {
	case "pattern":
		idx, expected := item.index, item.value
		return m, m.do(fmt.Sprintf("allowlist pattern #%d removed", idx), func(c context.Context) error {
			return app.RemoveAllowlistPattern(c, idx, expected)
		})
	case "source":
		idx, expected := item.index, item.value
		return m, m.do(fmt.Sprintf("task source #%d removed", idx), func(c context.Context) error {
			return app.RemoveTaskSource(c, idx, expected)
		})
	default:
		m.message = "config fields are edited (enter), not removed"
		return m, nil
	}
}

func (m Model) clearDataPrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: "type 'yes' to permanently clear learned history + audit data",
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if input != "yes" {
					return actionResultMsg{message: "clear-data aborted"}
				}
				if err := app.ClearData(ctx); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: "learned history and audit data cleared"}
			}
		},
	}
	return m, nil
}

// --- View ---

// View renders the pane.
func (m Model) View() string {
	var b strings.Builder

	state := runningStyle.Render("● running")
	if m.data.status.Paused {
		state = pausedStyle.Render("■ PAUSED (kill switch)")
	}
	fmt.Fprintf(&b, "%s  %s\n", titleStyle.Render("Herd Auto Prompter"), state)

	var tabs []string
	for i, name := range tabNames {
		label := fmt.Sprintf(" %s ", name)
		if i == int(tabEscalations) && len(m.data.escalations) > 0 {
			label = fmt.Sprintf(" %s(%d) ", name, len(m.data.escalations))
		}
		if tab(i) == m.tab {
			tabs = append(tabs, activeTab.Render(label))
		} else {
			tabs = append(tabs, inactiveTab.Render(label))
		}
	}
	fmt.Fprintf(&b, "%s\n\n", strings.Join(tabs, "|"))

	if m.data.err != nil {
		fmt.Fprintf(&b, "%s\n", errStyle.Render("error: "+m.data.err.Error()))
	}

	switch m.tab {
	case tabAgents:
		m.renderAgents(&b)
	case tabEscalations:
		m.renderEscalations(&b)
	case tabAudit:
		m.renderAudit(&b)
	case tabRules:
		m.renderRules(&b)
	case tabKill:
		m.renderKills(&b)
	}

	if m.prompt != nil {
		fmt.Fprintf(&b, "\n%s> %s█\n", m.prompt.label, m.prompt.input)
	}
	if m.message != "" {
		fmt.Fprintf(&b, "\n%s\n", m.message)
	}
	fmt.Fprintf(&b, "\n%s", helpStyle.Render(m.helpLine()))
	return b.String()
}

func (m Model) helpLine() string {
	common := "tab: switch  ↑/↓: select  p: pause  r: resume  q: quit"
	switch m.tab {
	case tabAgents:
		return "n: rename agent  " + common
	case tabEscalations:
		return "enter/y: confirm+send  c: correct  " + common
	case tabAudit:
		return "c: correct decision  " + common
	case tabRules:
		return "enter/e: edit field  a: add pattern  t: add task source  x: remove  X: clear data  " + common
	}
	return common
}

func (m Model) renderAgents(b *strings.Builder) {
	agents := m.data.status.MonitoredAgents
	if len(agents) == 0 {
		fmt.Fprintln(b, "no agents detected")
		return
	}
	for i, a := range agents {
		name := orDash(m.data.status.AgentName(a.AgentID))
		line := fmt.Sprintf("%-18s %-12s %-12s %s", name, a.AgentID, a.AgentType, a.Status)
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		fmt.Fprintln(b, line)
	}
}

func (m Model) renderEscalations(b *strings.Builder) {
	esc := m.data.escalations
	if len(esc) == 0 {
		fmt.Fprintln(b, "no pending escalations — the herd is unblocked 🎉")
		return
	}
	for i, e := range esc {
		agent := e.AgentID
		if n := m.data.status.AgentName(e.AgentID); n != "" {
			agent = n
		}
		line := fmt.Sprintf("#%-5d %-8s %-10s agent=%-14s %s",
			e.ID, e.CreatedAt.Format("15:04:05"), e.SituationType, agent, oneLine(e.Rationale, 60))
		if e.Suggestion != "" {
			line += "  → " + oneLine(e.Suggestion, 40)
		}
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		fmt.Fprintln(b, line)
	}
}

func (m Model) renderAudit(b *strings.Builder) {
	for i, r := range m.data.audit {
		line := fmt.Sprintf("#%-5d %-14s %-9s %-10s conf=%.2f %s",
			r.ID, r.CreatedAt.Format("01-02 15:04:05"), r.Status, r.SituationType,
			r.Confidence, oneLine(r.Action, 50))
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	if len(m.data.audit) == 0 {
		fmt.Fprintln(b, "no audit records yet")
	}
}

func (m Model) renderRules(b *strings.Builder) {
	if len(m.items) == 0 {
		fmt.Fprintln(b, "no configuration loaded")
		return
	}
	lastKind := ""
	for i, item := range m.items {
		if item.kind != lastKind {
			lastKind = item.kind
			switch item.kind {
			case "field":
				fmt.Fprintln(b, sectionStyle.Render("Config"))
			case "pattern":
				fmt.Fprintf(b, "\n%s\n", sectionStyle.Render(fmt.Sprintf(
					"Never-auto allowlist (operator patterns; +%d seed)", len(domain.SeedAllowlistPatterns))))
			case "source":
				fmt.Fprintf(b, "\n%s\n", sectionStyle.Render("Task sources"))
			}
		}
		line := "  " + item.label
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	if len(m.data.cfg.Safety.AllowlistPatterns) == 0 {
		fmt.Fprintf(b, "\n%s\n", sectionStyle.Render(fmt.Sprintf(
			"Never-auto allowlist: no operator patterns (+%d seed active) — press a to add",
			len(domain.SeedAllowlistPatterns))))
	}
	if len(m.data.cfg.TaskSources) == 0 {
		fmt.Fprintf(b, "%s\n", sectionStyle.Render("Task sources: none — press t to add"))
	}
}

func (m Model) renderKills(b *strings.Builder) {
	if len(m.data.kills) == 0 {
		fmt.Fprintln(b, "no pause/kill events recorded")
		return
	}
	for i, e := range m.data.kills {
		line := fmt.Sprintf("#%-4d %-20s %-8s by %s",
			e.ID, e.CreatedAt.Format(time.RFC3339), e.State, e.Author)
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		fmt.Fprintln(b, line)
	}
}

func oneLine(s string, limit int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Run starts the TUI program.
func Run(ctx context.Context, app *frontend.App) error {
	p := tea.NewProgram(New(ctx, app), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
