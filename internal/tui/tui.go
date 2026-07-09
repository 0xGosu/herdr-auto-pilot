// Package tui is the primary control surface, run as a Herdr pane. It
// surfaces monitored agents, pending escalations, the audit log, rules and
// thresholds, and the pause/kill switch with history (FR-022, FR-017,
// FR-021) — every capability here is also a CLI verb.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	helpStyle     = lipgloss.NewStyle().Faint(true)
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

type refreshMsg struct {
	status      frontend.Status
	escalations []domain.AuditRecord
	audit       []domain.AuditRecord
	kills       []domain.KillEvent
	err         error
}

type tickMsg time.Time

// Model is the Bubble Tea model.
type Model struct {
	app *frontend.App
	ctx context.Context

	tab      tab
	cursor   int
	data     refreshMsg
	message  string
	entering bool   // typing a correction
	input    string // correction text
	width    int
	height   int
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
	return msg
}

// Update handles events.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case refreshMsg:
		m.data = msg
		if m.cursor >= m.rowCount() {
			m.cursor = max(0, m.rowCount()-1)
		}
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.entering {
		switch msg.Type {
		case tea.KeyEnter:
			m.entering = false
			input := strings.TrimSpace(m.input)
			m.input = ""
			if input == "" {
				m.message = "correction cancelled"
				return m, nil
			}
			return m, m.doResolve(input)
		case tea.KeyEsc:
			m.entering = false
			m.input = ""
			m.message = "correction cancelled"
			return m, nil
		case tea.KeyBackspace:
			if r := []rune(m.input); len(r) > 0 {
				m.input = string(r[:len(r)-1])
			}
			return m, nil
		case tea.KeySpace:
			m.input += " "
			return m, nil
		case tea.KeyRunes:
			// Only printable input; key names like "up"/"home" must not
			// leak into the correction text.
			m.input += string(msg.Runes)
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
	case "shift+tab", "left", "h":
		m.tab = (m.tab + tabCount - 1) % tabCount
		m.cursor = 0
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < m.rowCount()-1 {
			m.cursor++
		}
	case "p":
		return m, m.doPause()
	case "r":
		return m, m.doResume()
	case "enter", "y":
		if m.tab == tabEscalations {
			return m, m.doConfirm()
		}
	case "c":
		if m.tab == tabEscalations || m.tab == tabAudit {
			m.entering = true
			m.input = ""
			m.message = ""
		}
	}
	return m, nil
}

func (m Model) rowCount() int {
	switch m.tab {
	case tabAgents:
		return len(m.data.status.MonitoredAgents)
	case tabEscalations:
		return len(m.data.escalations)
	case tabAudit:
		return len(m.data.audit)
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

func (m Model) doPause() tea.Cmd {
	app, ctx := m.app, m.ctx
	return func() tea.Msg {
		if err := app.Pause(ctx); err != nil {
			return refreshMsg{err: err}
		}
		return refreshData(ctx, app)
	}
}

func (m Model) doResume() tea.Cmd {
	app, ctx := m.app, m.ctx
	return func() tea.Msg {
		if err := app.Resume(ctx); err != nil {
			return refreshMsg{err: err}
		}
		return refreshData(ctx, app)
	}
}

func (m Model) doConfirm() tea.Cmd {
	rec := m.selectedAudit()
	if rec == nil {
		return nil
	}
	app, ctx, id := m.app, m.ctx, rec.ID
	return func() tea.Msg {
		if err := app.Confirm(ctx, id, true); err != nil {
			return refreshMsg{err: err}
		}
		return refreshData(ctx, app)
	}
}

func (m Model) doResolve(action string) tea.Cmd {
	rec := m.selectedAudit()
	if rec == nil {
		return nil
	}
	app, ctx, id := m.app, m.ctx, rec.ID
	return func() tea.Msg {
		if err := app.Resolve(ctx, id, action, false); err != nil {
			return refreshMsg{err: err}
		}
		return refreshData(ctx, app)
	}
}

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
	b.WriteString(strings.Join(tabs, "|") + "\n\n")

	if m.data.err != nil {
		b.WriteString(errStyle.Render("error: "+m.data.err.Error()) + "\n")
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

	if m.entering {
		fmt.Fprintf(&b, "\ncorrection> %s█\n", m.input)
	}
	if m.message != "" {
		b.WriteString("\n" + m.message + "\n")
	}
	b.WriteString("\n" + helpStyle.Render(
		"tab: switch  ↑/↓: select  enter/y: confirm+send  c: correct  p: pause  r: resume  q: quit"))
	return b.String()
}

func (m Model) renderAgents(b *strings.Builder) {
	agents := m.data.status.MonitoredAgents
	if len(agents) == 0 {
		b.WriteString("no agents detected\n")
		return
	}
	for i, a := range agents {
		line := fmt.Sprintf("%-16s %-12s %s", a.AgentID, a.AgentType, a.Status)
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
}

func (m Model) renderEscalations(b *strings.Builder) {
	esc := m.data.escalations
	if len(esc) == 0 {
		b.WriteString("no pending escalations — the herd is unblocked 🎉\n")
		return
	}
	for i, e := range esc {
		line := fmt.Sprintf("#%-5d %-8s %-10s agent=%-10s %s",
			e.ID, e.CreatedAt.Format("15:04:05"), e.SituationType, e.AgentID, oneLine(e.Rationale, 60))
		if e.Suggestion != "" {
			line += "  → " + oneLine(e.Suggestion, 40)
		}
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")
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
		b.WriteString(line + "\n")
	}
	if len(m.data.audit) == 0 {
		b.WriteString("no audit records yet\n")
	}
}

func (m Model) renderRules(b *strings.Builder) {
	cfg, err := m.app.Config()
	if err != nil {
		b.WriteString(errStyle.Render("config error: "+err.Error()) + "\n")
		return
	}
	fmt.Fprintf(b, "thresholds  idle=%.2f approval=%.2f choice=%.2f error=%.2f inferred=%.2f\n",
		cfg.Thresholds.Idle, cfg.Thresholds.Approval, cfg.Thresholds.Choice,
		cfg.Thresholds.Error, cfg.Thresholds.InferredTaskBar)
	fmt.Fprintf(b, "graduation  N=%d consecutive confirmations\n", cfg.Learning.GraduationN)
	fmt.Fprintf(b, "limits      %d consecutive, %d/minute, %d error retries\n",
		cfg.Limits.MaxConsecutiveAutoPrompts, cfg.Limits.MaxAutoPromptsPerMinute, cfg.Limits.MaxErrorRetries)
	fmt.Fprintf(b, "llm         configured=%v auto_act=%v timeout=%ds\n",
		len(cfg.LLM.Command) > 0, cfg.LLM.AutoAct, cfg.LLM.TimeoutSeconds)
	fmt.Fprintf(b, "\nnever-auto allowlist: %d seed + %d operator patterns\n",
		len(domain.SeedAllowlistPatterns), len(cfg.Safety.AllowlistPatterns))
	for _, p := range cfg.Safety.AllowlistPatterns {
		fmt.Fprintf(b, "  operator: %s\n", p)
	}
	b.WriteString(helpStyle.Render("\nedit via CLI: config set-threshold, rules add, task-source — or hand-edit config.toml"))
	b.WriteString("\n")
}

func (m Model) renderKills(b *strings.Builder) {
	if len(m.data.kills) == 0 {
		b.WriteString("no pause/kill events recorded\n")
		return
	}
	for i, e := range m.data.kills {
		line := fmt.Sprintf("#%-4d %-20s %-8s by %s",
			e.ID, e.CreatedAt.Format(time.RFC3339), e.State, e.Author)
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
}

func oneLine(s string, limit int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return s
}

// Run starts the TUI program.
func Run(ctx context.Context, app *frontend.App) error {
	p := tea.NewProgram(New(ctx, app), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
