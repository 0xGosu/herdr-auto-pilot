// Package tui is the primary control surface, run as a Herdr pane. It
// mirrors every CLI capability (FR-022): monitored agents (with rename),
// pending escalations (confirm/correct), the audit log (post-hoc
// correction), learned signatures (Rules tab: inspect/filter/delete),
// configuration (Config tab: fields, never-auto patterns, task sources,
// clear-data), and the pause/kill switch with history.
package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

type tab int

const (
	tabAgents tab = iota
	tabEscalations
	tabAudit
	tabSignatures // "Rules": learned signatures (list/inspect/delete)
	tabConfig     // config fields, never-auto patterns, task sources
	tabKill
	tabCount
)

var tabNames = []string{"Agents", "Escalations", "Audit", "Rules", "Config", "Pause/Kill"}

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
	signatures  []frontend.SignatureRow
	cfg         config.Config
	err         error
}

// sigDetailMsg carries an asynchronously loaded signature detail.
type sigDetailMsg struct {
	row     frontend.SignatureRow
	history []domain.DecisionRecord
	err     error
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

// detailView is a full-record overlay opened with `v` on the Agents,
// Escalations, Audit, and Rules tabs, showing the selected row untruncated.
type detailView struct {
	title  string
	lines  []string                 // wrapped to the pane width at open/resize
	offset int                      // first visible line (↑/↓ scroll)
	build  func(width int) []string // rebuilds lines from the snapshot on resize
	// confirmID is the escalation's audit id captured at open time, so
	// enter confirms the record ON SCREEN even if a background refresh
	// clamped the list cursor. 0 = not a confirmable escalation detail.
	confirmID int64
}

// ruleItem is one navigable row of the Config tab.
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
	items   []ruleItem  // Config tab rows, rebuilt on refresh
	sigMode domain.Mode // Rules tab display filter: "" = all
	message string
	prompt  *prompt
	detail  *detailView
	width   int
	height  int
}

// visibleSignatures applies the display-side mode filter (f key).
func (m Model) visibleSignatures() []frontend.SignatureRow {
	if m.sigMode == "" {
		return m.data.signatures
	}
	var out []frontend.SignatureRow
	for _, r := range m.data.signatures {
		if r.Mode == m.sigMode {
			out = append(out, r)
		}
	}
	return out
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
	msg.signatures, msg.err = app.Signatures(ctx, domain.SignatureFilter{})
	if msg.err != nil {
		return msg
	}
	msg.cfg, msg.err = app.Config()
	return msg
}

// buildRuleItems lays out the Config tab rows from the current config.
func buildRuleItems(cfg config.Config) []ruleItem {
	var items []ruleItem
	for _, key := range frontend.ConfigFieldKeys {
		items = append(items, ruleItem{
			kind: "field", key: key,
			label: fmt.Sprintf("%-38s %s", key, frontend.FieldValue(cfg, key)),
		})
	}
	for i, p := range cfg.Safety.NeverAutoPatterns {
		items = append(items, ruleItem{
			kind: "pattern", index: i, value: p,
			label: fmt.Sprintf("never-auto #%d  %s", i, p),
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
		label := fmt.Sprintf("task-source #%d  agent=%s ws=%s  %s", i, sel, ws, src.Path)
		if src.NextTaskTemplate != "" {
			label += fmt.Sprintf("  template=%q", src.NextTaskTemplate)
		}
		items = append(items, ruleItem{
			kind: "source", index: i, value: src.Path,
			label: label,
		})
	}
	return items
}

// Update handles events.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.detail != nil && m.detail.build != nil {
			m.detail.lines = m.detail.build(m.wrapWidth())
			m.detail.offset = min(m.detail.offset, max(0, len(m.detail.lines)-m.detailPageSize()))
		}
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
	case sigDetailMsg:
		if msg.err != nil {
			m.message = errStyle.Render(msg.err.Error())
			return m, nil
		}
		gradN := m.data.cfg.Learning.GraduationN
		build := func(width int) []string {
			return signatureDetailLines(msg.row, msg.history, gradN, width)
		}
		m.detail = &detailView{
			title: fmt.Sprintf("Signature %s", shortSig(msg.row.Signature)),
			lines: build(m.wrapWidth()),
			build: build,
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.detail != nil {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.detail.offset > 0 {
				m.detail.offset--
			}
		case "down", "j":
			if m.detail.offset < max(0, len(m.detail.lines)-m.detailPageSize()) {
				m.detail.offset++
			}
		case "tab", "right", "l":
			// Tab-switching works from inside the overlay: close it and
			// move on, no extra esc needed.
			m.detail = nil
			m.tab = (m.tab + 1) % tabCount
			m.cursor = 0
			m.message = ""
		case "shift+tab", "left", "h":
			m.detail = nil
			m.tab = (m.tab + tabCount - 1) % tabCount
			m.cursor = 0
			m.message = ""
		case "enter":
			// On an escalation's detail view, Enter confirms+sends the
			// record shown (by its snapshotted id, not the live cursor) and
			// returns to the list — no need to close and re-press.
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.confirmAuditID(id)
			}
			m.detail = nil
		case "esc", "q", "v":
			m.detail = nil
		}
		return m, nil
	}

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
		case tabSignatures:
			return m.viewSignatureDetail()
		case tabConfig:
			return m.editSelectedRule()
		}
	case "e":
		if m.tab == tabConfig {
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
	case "v":
		if m.tab == tabSignatures {
			return m.viewSignatureDetail()
		}
		return m.viewSelected()
	case "f":
		if m.tab == tabSignatures {
			switch m.sigMode {
			case "":
				m.sigMode = domain.ModeShadow
			case domain.ModeShadow:
				m.sigMode = domain.ModeAutonomous
			default:
				m.sigMode = ""
			}
			m.cursor = 0
			m.message = "filter: " + orDash(string(m.sigMode))
		}
	case "a":
		if m.tab == tabConfig {
			return m.addPatternPrompt()
		}
	case "t":
		if m.tab == tabConfig {
			return m.addTaskSourcePrompt()
		}
	case "x", "delete":
		switch m.tab {
		case tabSignatures:
			return m.deleteSignaturePrompt()
		case tabConfig:
			return m.removeSelectedRule()
		}
	case "X":
		if m.tab == tabConfig {
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
	case tabSignatures:
		return len(m.visibleSignatures())
	case tabConfig:
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
	return m.confirmAuditID(rec.ID)
}

// confirmAuditID confirms+sends a specific escalation by id (used by the
// list and by the detail overlay, which confirms the record it snapshotted).
func (m Model) confirmAuditID(id int64) (tea.Model, tea.Cmd) {
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

// --- Detail view (v) ---

// viewSelected opens a full-record overlay for the selected row. The record
// is snapshotted at open time; the build closure re-wraps it on resize.
func (m Model) viewSelected() (tea.Model, tea.Cmd) {
	switch m.tab {
	case tabAgents:
		if m.cursor < len(m.data.status.MonitoredAgents) {
			a := m.data.status.MonitoredAgents[m.cursor]
			build := func(width int) []string { return m.agentDetailLines(a, width) }
			m.message = ""
			m.detail = &detailView{
				title: fmt.Sprintf("Agent %s", a.AgentID),
				lines: build(m.wrapWidth()),
				build: build,
			}
		}
	case tabEscalations, tabAudit:
		if rec := m.selectedAudit(); rec != nil {
			kind := "Audit record"
			if m.tab == tabEscalations {
				kind = "Escalation"
			}
			r := *rec
			build := func(width int) []string { return m.auditDetailLines(r, width) }
			m.message = ""
			d := &detailView{
				title: fmt.Sprintf("%s #%d", kind, r.ID),
				lines: build(m.wrapWidth()),
				build: build,
			}
			// Only the Escalations detail is confirmable via enter.
			if m.tab == tabEscalations {
				d.confirmID = r.ID
			}
			m.detail = d
		}
	}
	return m, nil
}

// detailPageSize is how many detail lines fit under the header and help:
// header title + tabs + blank (3), detail title + blank (2), the more-lines
// indicator (1), and blank + help (2) — plus the error line when present.
func (m Model) detailPageSize() int {
	if m.height <= 0 {
		return 20
	}
	chrome := 8
	if m.data.err != nil {
		chrome++
	}
	return max(1, m.height-chrome)
}

// wrapWidth is the text width for the detail view.
func (m Model) wrapWidth() int {
	if m.width <= 0 {
		return 76
	}
	return max(40, m.width-4)
}

// fallbackContentWidth is used before the first WindowSizeMsg arrives: 1.5×
// the legacy fixed caps so even a pre-resize frame shows more than before.
const fallbackContentWidth = 120

// contentWidth is the usable width for list rows: the full terminal width by
// default, optionally capped by [tui] max_content_width, floored so a narrow
// terminal stays readable.
func (m Model) contentWidth() int {
	w := m.width
	if w <= 0 {
		w = fallbackContentWidth
	}
	if maxW := m.data.cfg.TUI.MaxContentWidth; maxW > 0 && maxW < w {
		w = maxW
	}
	return max(40, w)
}

// budgetSeparator is the width of the "  → " glyph the escalations row
// inserts between the rationale and the suggestion; budget() reserves it so a
// full row never overflows contentWidth and wraps.
const budgetSeparator = 4

// budget splits the width remaining after a fixed-width row prefix between a
// primary field (rationale/action) and an optional trailing field
// (suggestion). primary is favored; trailing gets at most 40%.
func (m Model) budget(prefixCells int, hasTrailing bool) (primary, trailing int) {
	remaining := m.contentWidth() - prefixCells
	if remaining < 20 {
		remaining = 20
	}
	if !hasTrailing {
		return remaining, 0
	}
	remaining -= budgetSeparator
	trailing = remaining * 2 / 5
	if trailing < 16 {
		trailing = 16
	}
	return remaining - trailing, trailing
}

// detailField appends a labelled, wrapped block; empty values are skipped.
func detailField(lines []string, width int, label, value string) []string {
	if strings.TrimSpace(value) == "" {
		return lines
	}
	lines = append(lines, sectionStyle.Render(label))
	for _, ln := range wrapText(value, width) {
		lines = append(lines, "  "+ln)
	}
	return lines
}

func (m Model) agentDetailLines(a domain.AgentTransition, w int) []string {
	var lines []string
	lines = detailField(lines, w, "Short name", orDash(m.data.status.AgentName(a.AgentID)))
	lines = detailField(lines, w, "Agent id", a.AgentID)
	lines = detailField(lines, w, "Workspace", locationLabel(a.WorkspaceID,
		func() (string, int, bool) {
			ws, ok := m.data.status.Workspaces[a.WorkspaceID]
			return ws.Label, ws.Number, ok
		}))
	lines = detailField(lines, w, "Tab", locationLabel(a.TabID,
		func() (string, int, bool) {
			tab, ok := m.data.status.Tabs[a.TabID]
			return tab.Label, tab.Number, ok
		}))
	lines = detailField(lines, w, "Pane", a.PaneID)
	lines = detailField(lines, w, "Type", a.AgentType)
	lines = detailField(lines, w, "Status", a.Status)
	if !a.At.IsZero() {
		lines = detailField(lines, w, "Last transition", a.At.Format(time.RFC3339))
	}
	return lines
}

// locationLabel renders `#<number> "<label>" (<id>)` for a workspace/tab,
// degrading to the raw id when no metadata is known.
func locationLabel(id string, lookup func() (label string, number int, ok bool)) string {
	if id == "" {
		return ""
	}
	if label, number, ok := lookup(); ok {
		out := fmt.Sprintf("#%d", number)
		if label != "" {
			out += fmt.Sprintf(" %q", label)
		}
		return out + " (" + id + ")"
	}
	return id
}

func (m Model) auditDetailLines(r domain.AuditRecord, w int) []string {
	var lines []string
	agent := r.AgentID
	if n := m.data.status.AgentName(r.AgentID); n != "" {
		agent = fmt.Sprintf("%s (%s)", n, r.AgentID)
	}
	lines = detailField(lines, w, "When", r.CreatedAt.Format(time.RFC3339))
	lines = detailField(lines, w, "Status", r.Status)
	lines = detailField(lines, w, "Situation", string(r.SituationType))
	lines = detailField(lines, w, "Agent", agent)
	lines = detailField(lines, w, "Agent type", m.agentTypeFor(r))
	lines = detailField(lines, w, "Confidence", fmt.Sprintf("%.2f", r.Confidence))
	lines = detailField(lines, w, "Trigger", r.Trigger)
	lines = detailField(lines, w, "Suggestion", r.Suggestion)
	lines = detailField(lines, w, "Action", r.Action)
	lines = detailField(lines, w, "Input", r.Input)
	lines = detailField(lines, w, "Rationale", r.Rationale)
	lines = detailField(lines, w, "LLM output", r.LLMOutput)
	lines = detailField(lines, w, "Signature", r.Signature)
	if r.Signature != "" {
		if row, ok := m.ruleFor(r.Signature); ok {
			lines = detailField(lines, w, "Matched rule",
				frontend.RuleSummary(row, m.data.cfg.Learning.GraduationN))
		} else {
			lines = detailField(lines, w, "Matched rule",
				"none yet — learned when the operator confirms or resolves this")
		}
	}
	if r.DecisionID != 0 {
		lines = detailField(lines, w, "Decision id", fmt.Sprintf("%d", r.DecisionID))
	}
	if r.CorrectsAuditID != 0 {
		lines = detailField(lines, w, "Corrects audit", fmt.Sprintf("#%d", r.CorrectsAuditID))
	}
	return lines
}

// ansiEscape matches CSI/OSC/charset-designation terminal escape sequences
// so raw CLI output (the LLM output field) cannot restyle or reposition the
// pane; controlChars then strips any leftover C0 controls (except newline).
var (
	ansiEscape   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b\[[0-9;?]*[a-zA-Z]|\x1b[()#][0-9A-Za-z]|\x1b[@-_]`)
	controlChars = regexp.MustCompile("[\x00-\x08\x0b-\x1f\x7f]")
)

// wrapText wraps s at width display cells, preserving existing newlines.
// Escape sequences, carriage returns, and tabs are sanitized first: values
// can be verbatim subprocess output that would otherwise overprint the
// screen. Cell-width wrapping (not rune count) keeps wide runes (CJK,
// emoji) from overflowing the pane and breaking the row budget.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	s = ansiEscape.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	s = controlChars.ReplaceAllString(s, "")
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		cur := make([]rune, 0, width)
		cells := 0
		for _, r := range ln {
			w := runewidth.RuneWidth(r)
			if cells+w > width && len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
				cells = 0
			}
			cur = append(cur, r)
			cells += w
		}
		out = append(out, string(cur))
	}
	return out
}

// --- Signatures (Rules tab) ---

func (m Model) selectedSignature() *frontend.SignatureRow {
	sigs := m.visibleSignatures()
	if m.tab == tabSignatures && m.cursor < len(sigs) {
		return &sigs[m.cursor]
	}
	return nil
}

// viewSignatureDetail loads the full record (history + last audit) off the
// Update loop and opens the detail overlay when it arrives.
func (m Model) viewSignatureDetail() (tea.Model, tea.Cmd) {
	row := m.selectedSignature()
	if row == nil {
		return m, nil
	}
	sig := row.Signature
	app, ctx := m.app, m.ctx
	m.message = ""
	return m, func() tea.Msg {
		detail, history, err := app.SignatureDetail(ctx, sig)
		return sigDetailMsg{row: detail, history: history, err: err}
	}
}

func (m Model) deleteSignaturePrompt() (tea.Model, tea.Cmd) {
	row := m.selectedSignature()
	if row == nil {
		return m, nil
	}
	sig, decisions := row.Signature, row.Decisions
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: fmt.Sprintf("type 'yes' to delete %s and its %d decision(s)", shortSig(sig), decisions),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if input != "yes" {
					return actionResultMsg{message: "delete aborted"}
				}
				deleted, n, err := app.DeleteSignature(ctx, sig)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf(
					"deleted %s and %d decision(s); audit rows kept", shortSig(deleted), n)}
			}
		},
	}
	return m, nil
}

// signatureDetailLines renders the full-record overlay for one signature.
func signatureDetailLines(row frontend.SignatureRow, history []domain.DecisionRecord, graduationN, w int) []string {
	var lines []string
	lines = detailField(lines, w, "Signature", row.Signature)
	lines = detailField(lines, w, "Situation", string(row.SituationType))
	lines = detailField(lines, w, "Agent type", orDash(row.AgentType))
	lines = detailField(lines, w, "Mode", string(row.Mode))
	lines = detailField(lines, w, "Streak", fmt.Sprintf("%d/%d confirmations toward graduation", row.ConsecutiveConfirmations, graduationN))
	lines = detailField(lines, w, "Confidence", fmt.Sprintf("%.2f (cached)", row.CachedConfidence))
	if row.TopAction != "" {
		lines = detailField(lines, w, "Top action", fmt.Sprintf("%q over %d decision(s)", row.TopAction, row.Decisions))
	}
	if row.GuardState != "" {
		lines = detailField(lines, w, "Guard", row.GuardState)
	}
	if !row.UpdatedAt.IsZero() {
		lines = detailField(lines, w, "Updated", row.UpdatedAt.Format(time.RFC3339))
	}
	// Rule provenance: what the pane showed when this signature was first
	// seen — the situation the learned action answers.
	if row.PaneExcerpt != "" {
		lines = detailField(lines, w, "Original situation", row.PaneExcerpt)
	} else {
		lines = detailField(lines, w, "Original situation", "(not captured yet — recorded on the rule's next sighting)")
	}
	if len(history) > 0 {
		var b strings.Builder
		for _, d := range history {
			marker := ""
			if d.IsCorrection {
				marker = "  CORRECTION"
			}
			fmt.Fprintf(&b, "#%d  %s  %q  source=%s%s\n",
				d.ID, d.CreatedAt.Format("01-02 15:04:05"), d.ChosenAction, d.Source, marker)
		}
		lines = detailField(lines, w, "Recent decisions (newest first)", strings.TrimRight(b.String(), "\n"))
	}
	if a := row.LastAudit; a != nil {
		lines = detailField(lines, w, "Last audit",
			fmt.Sprintf("#%d (%s) %s — %s", a.ID, a.Status, a.Action, a.Rationale))
	}
	return lines
}

// shortSig abbreviates a signature hash for one-line rows.
func shortSig(sig string) string {
	if len(sig) <= 16 {
		return sig
	}
	return sig[:16] + "…"
}

// --- Config tab editing ---

func (m Model) selectedRule() *ruleItem {
	if m.tab == tabConfig && m.cursor < len(m.items) {
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
		label: "add never-auto regex",
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if err := app.AddNeverAutoPattern(ctx, input); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: "never-auto pattern added"}
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
				if err := app.AddTaskSource(ctx, agent, workspace, parts[0], ""); err != nil {
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
		return m, m.do(fmt.Sprintf("never-auto pattern #%d removed", idx), func(c context.Context) error {
			return app.RemoveNeverAutoPattern(c, idx, expected)
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

	if m.detail != nil {
		m.renderDetail(&b)
		return b.String()
	}

	switch m.tab {
	case tabAgents:
		m.renderAgents(&b)
	case tabEscalations:
		m.renderEscalations(&b)
	case tabAudit:
		m.renderAudit(&b)
	case tabSignatures:
		m.renderSignatures(&b)
	case tabConfig:
		m.renderConfig(&b)
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
	if m.detail != nil {
		if m.detail.confirmID != 0 {
			return "enter: confirm+send  ↑/↓: scroll  tab: switch tab  esc/q/v: close"
		}
		return "↑/↓: scroll  tab: switch tab  esc/q/v: close"
	}
	common := "tab: switch  ↑/↓: select  p: pause  r: resume  q: quit"
	switch m.tab {
	case tabAgents:
		return "v: details  n: rename agent  " + common
	case tabEscalations:
		return "enter/y: confirm+send  c: correct  v: details  " + common
	case tabAudit:
		return "c: correct decision  v: details  " + common
	case tabSignatures:
		return "enter/v: details  x: delete  f: filter mode  " + common
	case tabConfig:
		return "enter/e: edit field  a: add pattern  t: add task source  x: remove  X: clear data  " + common
	}
	return common
}

// renderSignatures draws the learned-signature list (the Rules tab).
func (m Model) renderSignatures(b *strings.Builder) {
	sigs := m.visibleSignatures()
	if m.sigMode != "" {
		fmt.Fprintf(b, "%s\n", sectionStyle.Render("filter: mode="+string(m.sigMode)+"  (f cycles)"))
	}
	if len(sigs) == 0 {
		if len(m.data.signatures) > 0 {
			fmt.Fprintln(b, "no signatures match the filter — press f to cycle")
			return
		}
		fmt.Fprintln(b, "no learned signatures yet — confirm suggestions to teach hap")
		return
	}
	gradN := m.data.cfg.Learning.GraduationN
	// Prefix up to the action column is ~66 fixed cells.
	actWidth, _ := m.budget(66, false)
	for i, r := range sigs {
		line := fmt.Sprintf("%-18s %-9s %-10s %-11s %d/%d conf=%.2f  %s",
			shortSig(r.Signature), r.SituationType, orDash(r.AgentType), r.Mode,
			r.ConsecutiveConfirmations, gradN, r.CachedConfidence,
			oneLine(r.TopAction, actWidth))
		switch {
		case i == m.cursor:
			line = selectedStyle.Render(line)
		case r.Mode == domain.ModeAutonomous:
			line = okStyle.Render(line)
		}
		fmt.Fprintln(b, line)
	}
}

// renderDetail draws the open detail overlay in place of the tab body.
func (m Model) renderDetail(b *strings.Builder) {
	fmt.Fprintf(b, "%s\n\n", titleStyle.Render(m.detail.title))
	page := m.detailPageSize()
	lines := m.detail.lines
	start := min(m.detail.offset, max(0, len(lines)-1))
	end := min(start+page, len(lines))
	for _, ln := range lines[start:end] {
		fmt.Fprintln(b, ln)
	}
	if end < len(lines) {
		fmt.Fprintf(b, "%s\n", helpStyle.Render(fmt.Sprintf("… %d more line(s) — ↓ to scroll", len(lines)-end)))
	}
	fmt.Fprintf(b, "\n%s", helpStyle.Render(m.helpLine()))
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

// ruleFor resolves the learned rule an audit/escalation row is keyed to
// (they share the signature string), from the snapshot the Rules tab uses.
func (m Model) ruleFor(signature string) (frontend.SignatureRow, bool) {
	if signature == "" {
		return frontend.SignatureRow{}, false
	}
	for _, row := range m.data.signatures {
		if row.Signature == signature {
			return row, true
		}
	}
	return frontend.SignatureRow{}, false
}

// ruleMarker is the compact list-column form of ruleFor: the rule's mode
// abbreviated, or "-" when no rule exists yet.
func (m Model) ruleMarker(signature string) string {
	row, ok := m.ruleFor(signature)
	if !ok {
		return "-"
	}
	if row.Mode == domain.ModeAutonomous {
		return "auto"
	}
	return string(row.Mode)
}

// agentTypeFor resolves an audit row's agent type: the recorded value, or —
// for rows written before the audit log carried it — the live agent's type.
func (m Model) agentTypeFor(r domain.AuditRecord) string {
	if r.AgentType != "" {
		return r.AgentType
	}
	for _, a := range m.data.status.MonitoredAgents {
		if a.AgentID == r.AgentID {
			return a.AgentType
		}
	}
	return ""
}

func (m Model) renderEscalations(b *strings.Builder) {
	esc := m.data.escalations
	if len(esc) == 0 {
		fmt.Fprintln(b, "no pending escalations — the herd is unblocked 🎉")
		return
	}
	// Prefix: "#%-5d %-8s %-10s %-8s agent=%-14s rule=%-6s " → 69 cells.
	const escPrefix = 69
	for i, e := range esc {
		agent := e.AgentID
		if n := m.data.status.AgentName(e.AgentID); n != "" {
			agent = n
		}
		rWidth, sWidth := m.budget(escPrefix, e.Suggestion != "")
		line := fmt.Sprintf("#%-5d %-8s %-10s %-8s agent=%-14s rule=%-6s %s",
			e.ID, e.CreatedAt.Format("15:04:05"), e.SituationType,
			oneLine(orDash(m.agentTypeFor(e)), 8), agent,
			m.ruleMarker(e.Signature), oneLine(e.Rationale, rWidth))
		if e.Suggestion != "" {
			line += "  → " + oneLine(e.Suggestion, sWidth)
		}
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		fmt.Fprintln(b, line)
	}
}

func (m Model) renderAudit(b *strings.Builder) {
	// Prefix up to the action column is ~53 fixed cells + "rule=%-6s " (12).
	actWidth, _ := m.budget(65, false)
	for i, r := range m.data.audit {
		line := fmt.Sprintf("#%-5d %-14s %-9s %-10s conf=%.2f rule=%-6s %s",
			r.ID, r.CreatedAt.Format("01-02 15:04:05"), r.Status, r.SituationType,
			r.Confidence, m.ruleMarker(r.Signature), oneLine(r.Action, actWidth))
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	if len(m.data.audit) == 0 {
		fmt.Fprintln(b, "no audit records yet")
	}
}

func (m Model) renderConfig(b *strings.Builder) {
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
					"Never-auto patterns (operator; +%d seed)", len(domain.SeedNeverAutoPatterns))))
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
	if len(m.data.cfg.Safety.NeverAutoPatterns) == 0 {
		fmt.Fprintf(b, "\n%s\n", sectionStyle.Render(fmt.Sprintf(
			"Never-auto patterns: none from operator (+%d seed active) — press a to add",
			len(domain.SeedNeverAutoPatterns))))
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
