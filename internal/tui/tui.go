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
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// isList reports whether t renders a scrollable, searchable row list.
// Config and Pause/Kill keep their existing unwindowed navigation (AR-032).
func (t tab) isList() bool {
	return t == tabAgents || t == tabEscalations || t == tabAudit || t == tabSignatures
}

type refreshMsg struct {
	status      frontend.Status
	escalations []domain.AuditRecord
	audit       []domain.AuditRecord
	kills       []domain.KillEvent
	signatures  []frontend.SignatureRow
	cfg         config.Config
	// daemonHealth combines lock + heartbeat + crash-loop state for the health
	// banner, so the operator sees a hung/degraded/crash-looping daemon that
	// otherwise looks identical to "all quiet" (no escalations).
	daemonHealth frontend.DaemonHealth
	// pendingConsult holds agent ids that currently have an LLM consult in
	// flight, so "l: retry LLM" is disabled while one is running (the daemon
	// re-checks authoritatively). Populated only for retryable escalations.
	pendingConsult map[string]bool
	err            error
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

// statusNote is a durable action outcome shown in the status area until
// the next outcome replaces it (CR-025) — unlike the transient m.message
// hint line, navigation never clears it.
type statusNote struct {
	text string
	err  bool
	at   time.Time
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
	// The per-entry actions (c/x/l) act on this id too, never the live
	// cursor. escRetryable snapshots whether "l: retry LLM" is offered.
	confirmID    int64
	escRetryable bool
}

// ruleItem is one navigable row of the Config tab. "indicator" and
// "capture" rows are read-only (AR-034, AR-035): they render for
// visibility and refuse edit/remove with a config.toml pointer.
type ruleItem struct {
	kind  string // "field" | "pattern" | "source" | "indicator" | "capture"
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
	items   []ruleItem     // Config tab rows, rebuilt on refresh
	sigMode domain.Mode    // Rules tab display filter: "" = all
	marked  map[int64]bool // Escalations tab multi-select (audit ids), space toggles
	message string
	prompt  *prompt
	detail  *detailView
	width   int
	height  int

	offsets   [tabCount]int    // per-list viewport offset (AR-001)
	query     [tabCount]string // per-tab search filter (AR-013)
	searching bool             // search-input mode on the active tab (AR-011)
	status    *statusNote      // durable action outcome (CR-025)
	st        *styles          // palette-resolved styles; nil = default palette
}

// matchesQuery reports whether any of the row's visible column values
// contains tab t's query as a case-insensitive substring (AR-013).
func (m Model) matchesQuery(t tab, fields ...string) bool {
	q := strings.ToLower(m.query[t])
	if q == "" {
		return true
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// visibleAgents applies the Agents tab search filter.
func (m Model) visibleAgents() []domain.AgentTransition {
	if m.query[tabAgents] == "" {
		return m.data.status.MonitoredAgents
	}
	var out []domain.AgentTransition
	for _, a := range m.data.status.MonitoredAgents {
		if m.matchesQuery(tabAgents, m.data.status.AgentName(a.AgentID),
			a.AgentID, a.AgentType, a.Status) {
			out = append(out, a)
		}
	}
	return out
}

// visibleEscalations applies the Escalations tab search filter.
func (m Model) visibleEscalations() []domain.AuditRecord {
	return m.filterAudit(tabEscalations, m.data.escalations)
}

// visibleAudit applies the Audit tab search filter.
func (m Model) visibleAudit() []domain.AuditRecord {
	return m.filterAudit(tabAudit, m.data.audit)
}

func (m Model) filterAudit(t tab, rows []domain.AuditRecord) []domain.AuditRecord {
	if m.query[t] == "" {
		return rows
	}
	var out []domain.AuditRecord
	for _, r := range rows {
		if m.matchesQuery(t,
			fmt.Sprintf("#%d", r.ID), string(r.SituationType), r.Status,
			m.data.status.AgentName(r.AgentID), r.AgentID, m.agentTypeFor(r),
			r.Action, r.Rationale, r.Suggestion) {
			out = append(out, r)
		}
	}
	return out
}

// visibleSignatures applies the display-side mode filter (f key) composed
// with the Rules tab search query (CR-017).
func (m Model) visibleSignatures() []frontend.SignatureRow {
	if m.sigMode == "" && m.query[tabSignatures] == "" {
		return m.data.signatures
	}
	var out []frontend.SignatureRow
	for _, r := range m.data.signatures {
		if m.sigMode != "" && r.Mode != m.sigMode {
			continue
		}
		if !m.matchesQuery(tabSignatures, r.Signature, string(r.SituationType),
			r.AgentType, string(r.Mode), r.TopAction) {
			continue
		}
		out = append(out, r)
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
	// Daemon health is read from local state files (never errors), so assess it
	// first — it stays meaningful even when GetStatus fails (e.g. daemon down).
	msg.daemonHealth = app.AssessDaemonHealth()
	msg.status, msg.err = app.GetStatus(ctx)
	if msg.err != nil {
		return msg
	}
	msg.escalations, msg.err = app.Escalations(ctx)
	if msg.err != nil {
		return msg
	}
	// Gate "retry LLM" per agent: a consult already in flight disables it.
	// Best-effort — a lookup error just leaves the key enabled (the daemon
	// guards authoritatively before re-consulting).
	msg.pendingConsult = map[string]bool{}
	checked := map[string]bool{}
	for i := range msg.escalations {
		e := msg.escalations[i]
		if !domain.IsRetryableLLMEscalation(&e) || checked[e.AgentID] {
			continue
		}
		checked[e.AgentID] = true
		if pending, perr := app.HasPendingLLMConsult(ctx, e.AgentID); perr == nil && pending {
			msg.pendingConsult[e.AgentID] = true
		}
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
	// Read-only visibility rows (AR-034, AR-035): the suspected-irreversible
	// indicator patterns and the capture-delay rules, previously invisible
	// outside config.toml.
	for i, p := range cfg.Safety.IrreversibleIndicators {
		items = append(items, ruleItem{
			kind: "indicator", index: i, value: p,
			label: fmt.Sprintf("indicator #%d  %s", i, p),
		})
	}
	for i, r := range cfg.Safety.IndicatorRules {
		scope := "*"
		if len(r.Agents) > 0 {
			scope = strings.Join(r.Agents, ",")
		}
		items = append(items, ruleItem{
			kind: "indicator", index: len(cfg.Safety.IrreversibleIndicators) + i, value: r.Pattern,
			label: fmt.Sprintf("indicator-rule #%d  agents=%s  %s", i, scope, r.Pattern),
		})
	}
	if len(cfg.CaptureDelays) == 0 {
		// No rules configured: show the effective built-in defaults so the
		// operator can see what timing applies (AR-035).
		items = append(items, ruleItem{
			kind: "capture",
			label: fmt.Sprintf("defaults  start=%dms event=%dms (built-in)",
				cfg.CaptureDelay("*", true).Milliseconds(), cfg.CaptureDelay("*", false).Milliseconds()),
		})
	}
	for i, r := range cfg.CaptureDelays {
		at := r.AgentType
		if at == "" {
			at = "*"
		}
		items = append(items, ruleItem{
			kind: "capture", index: i,
			label: fmt.Sprintf("capture-delay #%d  agent=%s start=%dms event=%dms",
				i, at, r.StartMs, r.EventMs),
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
		m.clampListViewport()
		return m, nil
	case refreshMsg:
		m.data = msg
		m.items = buildRuleItems(msg.cfg)
		// A failed refresh carries a zero config; keep the current palette
		// rather than flickering back to the default while the error shows.
		if msg.err == nil {
			st := newStyles(resolvePalette(msg.cfg.TUI))
			m.st = &st
		}
		m.clampListViewport()
		// Drop marks whose escalations are no longer pending (deleted,
		// confirmed, or resolved elsewhere) — marks track ids, not rows.
		if len(m.marked) > 0 {
			pending := make(map[int64]bool, len(msg.escalations))
			for _, e := range msg.escalations {
				pending[e.ID] = true
			}
			for id := range m.marked {
				if !pending[id] {
					delete(m.marked, id)
				}
			}
		}
		return m, nil
	case actionResultMsg:
		if msg.err != nil {
			m.status = &statusNote{text: msg.err.Error(), err: true, at: time.Now()}
		} else if msg.message != "" {
			m.status = &statusNote{text: msg.message, at: time.Now()}
		}
		// The status area shrinks the page by 2 — keep the cursor visible.
		m.clampListViewport()
		return m, m.refresh()
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case sigDetailMsg:
		if msg.err != nil {
			m.status = &statusNote{text: msg.err.Error(), err: true, at: time.Now()}
			return m, nil
		}
		gradN := m.data.cfg.Learning.GraduationN
		build := func(width int) []string {
			return m.signatureDetailLines(msg.row, msg.history, gradN, width)
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
		case "tab", "right":
			// Tab-switching works from inside the overlay: close it and
			// move on, no extra esc needed. (On an escalation detail, `l` is
			// "retry LLM" instead — vim-right still switches via "right".)
			m.detail = nil
			m.tab = (m.tab + 1) % tabCount
			m.cursor = 0
			m.offsets[m.tab] = 0
			m.searching = false
			m.message = ""
		case "shift+tab", "left", "h":
			m.detail = nil
			m.tab = (m.tab + tabCount - 1) % tabCount
			m.cursor = 0
			m.offsets[m.tab] = 0
			m.searching = false
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
		case "c":
			// Per-entry actions mirror the list, acting on the snapshotted
			// escalation id (confirmID), never the live cursor.
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.correctByID(id)
			}
		case "x", "delete":
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.dismissByID(id)
			}
		case "l":
			// On an escalation detail, `l` retries the LLM on the snapshotted
			// record; elsewhere it keeps its vim-right "next tab" meaning.
			if id := m.detail.confirmID; id != 0 {
				if !m.detail.escRetryable {
					m.message = "retry LLM: not available for this escalation"
					m.detail = nil
					return m, nil
				}
				m.detail = nil
				return m.retryByID(id)
			}
			m.detail = nil
			m.tab = (m.tab + 1) % tabCount
			m.cursor = 0
			m.offsets[m.tab] = 0
			m.searching = false
			m.message = ""
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

	// Search-input mode (AR-011): every printable key edits the query —
	// action and navigation bindings, `q`/`y` included, never fire while
	// typing (CR-019). esc and enter both exit, retaining a non-empty
	// query as the active filter (AR-014, AR-015).
	if m.searching {
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEsc, tea.KeyEnter:
			m.searching = false
		case tea.KeyBackspace:
			if r := []rune(m.query[m.tab]); len(r) > 0 {
				m.query[m.tab] = string(r[:len(r)-1])
			}
		case tea.KeySpace:
			m.query[m.tab] += " "
		case tea.KeyRunes:
			m.query[m.tab] += string(msg.Runes)
		}
		m.clampListViewport() // CR-016
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "right", "l":
		// `l` retries the LLM on the Escalations tab; everywhere else it keeps
		// its vim-right "next tab" meaning ("tab"/"right" always switch).
		if msg.String() == "l" && m.tab == tabEscalations {
			return m.retrySelected()
		}
		m.tab = (m.tab + 1) % tabCount
		m.cursor = 0
		m.offsets[m.tab] = 0
		m.message = ""
	case "shift+tab", "left", "h":
		m.tab = (m.tab + tabCount - 1) % tabCount
		m.cursor = 0
		m.offsets[m.tab] = 0
		m.message = ""
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		m.scrollCursorIntoView()
		m.message = ""
	case "down", "j":
		if m.cursor < m.rowCount()-1 {
			m.cursor++
		}
		m.scrollCursorIntoView()
		m.message = ""
	case "/":
		if m.tab.isList() {
			m.searching = true
			m.message = ""
		}
	case "backspace":
		// The active-filter hint advertises backspace-to-clear outside
		// search mode too; a no-op otherwise.
		if m.tab.isList() && m.query[m.tab] != "" {
			m.query[m.tab] = ""
			m.clampListViewport()
		}
	case "p":
		return m, m.do("automation paused", func(ctx context.Context) error { return m.app.Pause(ctx) })
	case "r":
		return m, m.do("automation resumed", func(ctx context.Context) error { return m.app.Resume(ctx) })
	case "R":
		switch d := m.data.status.Drift; {
		case !d.Detected:
			m.message = "no embedding drift detected — rules already match the configured model"
		case d.ModelMissing:
			// Match the CLI's refusal: a re-embed cannot run without the
			// model file, so a "requested" toast would be a lie.
			m.message = "embedding model not found — fix embedding.model_path first"
		default:
			return m, m.do("re-compute requested — daemon is re-embedding in the background",
				func(ctx context.Context) error { return m.app.RequestReembed(ctx) })
		}
		return m, nil
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
	case "!":
		// Global: open the daemon's captured stderr (the crash reason behind an
		// error-severity banner) as a scrollable detail (#83).
		return m.viewDaemonStderr()
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
			m.offsets[tabSignatures] = 0
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
	case " ":
		if m.tab == tabEscalations {
			return m.toggleMarkSelected()
		}
	case "x", "delete":
		switch m.tab {
		case tabEscalations:
			return m.deleteEscalations()
		case tabSignatures:
			return m.deleteSignaturePrompt()
		case tabConfig:
			return m.removeSelectedRule()
		case tabAudit:
			m.message = "audit log is append-only — entries can't be deleted individually"
			m.scrollCursorIntoView() // the hint line shrinks the page
		}
	case "X":
		switch m.tab {
		case tabEscalations:
			return m.pruneEscalationsPrompt()
		case tabConfig:
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

// rowCountFor counts tab t's currently visible rows: search-filter-aware
// for the four list tabs (CR-008); Config and Pause/Kill keep their raw
// counts so their navigation is untouched (AR-032).
func (m Model) rowCountFor(t tab) int {
	switch t {
	case tabAgents:
		return len(m.visibleAgents())
	case tabEscalations:
		return len(m.visibleEscalations())
	case tabAudit:
		return len(m.visibleAudit())
	case tabSignatures:
		return len(m.visibleSignatures())
	case tabConfig:
		return len(m.items)
	case tabKill:
		return len(m.data.kills)
	}
	return 0
}

func (m Model) rowCount() int { return m.rowCountFor(m.tab) }

func (m Model) selectedAudit() *domain.AuditRecord {
	switch m.tab {
	case tabEscalations:
		if esc := m.visibleEscalations(); m.cursor < len(esc) {
			return &esc[m.cursor]
		}
	case tabAudit:
		if rows := m.visibleAudit(); m.cursor < len(rows) {
			return &rows[m.cursor]
		}
	}
	return nil
}

// canRetry reports whether "retry LLM" is offered for an escalation: it must
// be a retryable LLM failure (timeout / no-submit) with no consult currently
// in flight for its agent.
func (m Model) canRetry(rec domain.AuditRecord) bool {
	return domain.IsRetryableLLMEscalation(&rec) && !m.data.pendingConsult[rec.AgentID]
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
	return m.correctByID(rec.ID)
}

// correctByID opens the correction prompt for a specific audit id — used by
// the list and by the detail overlay (which corrects its snapshotted record,
// not the live cursor).
func (m Model) correctByID(id int64) (tea.Model, tea.Cmd) {
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

// dismissByID dismisses one escalation by id — used by the detail overlay.
// The list uses deleteEscalations for its marked/cursor batch semantics.
func (m Model) dismissByID(id int64) (tea.Model, tea.Cmd) {
	return m, m.do(fmt.Sprintf("dismissed #%d", id), func(ctx context.Context) error {
		return m.app.Dismiss(ctx, id)
	})
}

// retryByID re-invokes the LLM on one escalation by id (list and detail).
func (m Model) retryByID(id int64) (tea.Model, tea.Cmd) {
	return m, m.do(fmt.Sprintf("retry LLM queued for #%d", id), func(ctx context.Context) error {
		return m.app.RetryLLM(ctx, id)
	})
}

// retrySelected re-invokes the LLM on the escalation under the cursor, with a
// hint when the row isn't eligible or a consult is already running.
func (m Model) retrySelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	if !domain.IsRetryableLLMEscalation(rec) {
		m.message = "retry LLM: only for a failed or timed-out LLM escalation"
		return m, nil
	}
	if m.data.pendingConsult[rec.AgentID] {
		m.message = "retry LLM: a consult is already running for this agent"
		return m, nil
	}
	return m.retryByID(rec.ID)
}

// toggleMarkSelected flips the multi-select mark on the escalation under the
// cursor and advances, so repeated space marks a run of rows.
func (m Model) toggleMarkSelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	if m.marked == nil {
		m.marked = map[int64]bool{}
	}
	if m.marked[rec.ID] {
		delete(m.marked, rec.ID)
	} else {
		m.marked[rec.ID] = true
	}
	if len(m.marked) == 0 {
		m.message = "no marks — x deletes the row under the cursor"
	} else {
		m.message = fmt.Sprintf("%d marked — x deletes them", len(m.marked))
	}
	if m.cursor < m.rowCount()-1 {
		m.cursor++
	}
	// The advance can walk the cursor past the window's bottom edge (and
	// the mark message shrinks the page): keep the cursor row visible
	// (AR-003).
	m.scrollCursorIntoView()
	return m, nil
}

// deleteEscalationIDs is what x targets: the marked escalations (in list
// order), or just the cursor row when nothing is marked.
func (m Model) deleteEscalationIDs() []int64 {
	var ids []int64
	for _, e := range m.data.escalations {
		if m.marked[e.ID] {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		if rec := m.selectedAudit(); rec != nil {
			ids = append(ids, rec.ID)
		}
	}
	return ids
}

// describeEscalations names the delete targets compactly: "escalation #41"
// or "3 escalations (#41 #40 #39)", eliding a long id list.
func describeEscalations(ids []int64) string {
	if len(ids) == 1 {
		return fmt.Sprintf("escalation #%d", ids[0])
	}
	var parts []string
	for i, id := range ids {
		if i == 6 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("#%d", id))
	}
	return fmt.Sprintf("%d escalations (%s)", len(ids), strings.Join(parts, " "))
}

// deleteEscalations immediately dismisses the targeted escalations — no
// confirmation: dismissing is safe (nothing is sent or learned) and the
// audit rows are kept with status "dismissed".
func (m Model) deleteEscalations() (tea.Model, tea.Cmd) {
	ids := m.deleteEscalationIDs()
	if len(ids) == 0 {
		return m, nil
	}
	app, ctx := m.app, m.ctx
	desc := describeEscalations(ids)
	m.message = ""
	return m, func() tea.Msg {
		// Skip-and-continue: a failed id usually means the row was
		// resolved/confirmed concurrently; the rest still delete.
		deleted := 0
		var skipped []string
		var firstErr error
		for _, id := range ids {
			if err := app.Dismiss(ctx, id); err != nil {
				skipped = append(skipped, fmt.Sprintf("#%d", id))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			deleted++
		}
		if firstErr != nil {
			return actionResultMsg{err: fmt.Errorf("deleted %d, skipped %s: %w",
				deleted, strings.Join(skipped, " "), firstErr)}
		}
		return actionResultMsg{message: fmt.Sprintf(
			"deleted %s; audit rows kept as dismissed", desc)}
	}
}

// pruneEscalationsPrompt asks for an age in minutes (pre-filled with the
// default, editable) and dismisses every pending escalation older than that.
// Enter confirms, esc cancels.
func (m Model) pruneEscalationsPrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: "prune escalations older than N minutes — enter confirms, esc cancels",
		input: strconv.Itoa(frontend.DefaultPruneMinutes),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				minutes, err := strconv.Atoi(input)
				if err != nil || minutes <= 0 {
					return actionResultMsg{err: fmt.Errorf("invalid age %q — whole minutes", input)}
				}
				n, err := app.PruneEscalations(ctx, time.Duration(minutes)*time.Minute)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf(
					"pruned %d escalation(s) older than %d minute(s); audit rows kept as dismissed", n, minutes)}
			}
		},
	}
	return m, nil
}

// --- Agent rename ---

func (m Model) renameSelected() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursor >= len(agents) {
		return m, nil
	}
	agent := agents[m.cursor]
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
// viewDaemonStderr opens the daemon's captured stderr as a scrollable detail —
// the last-crash reason behind a hung/crash-looping/gave-up banner (#83). Only
// offered in an error state: a healthy daemon has no crash to explain.
func (m Model) viewDaemonStderr() (tea.Model, tea.Cmd) {
	if m.data.daemonHealth.Severity() != frontend.DaemonError {
		m.message = "daemon is not crashed/hung — no captured output to show"
		return m, nil
	}
	path, tail := m.app.DaemonStderrTail()
	build := func(width int) []string {
		var lines []string
		if path != "" {
			lines = append(lines, path, "")
		}
		if tail == "" {
			return append(lines, "(no captured stderr — the daemon left no output)")
		}
		for _, ln := range strings.Split(tail, "\n") {
			if ln == "" {
				lines = append(lines, "")
				continue
			}
			lines = append(lines, wrapText(ln, width)...)
		}
		return lines
	}
	m.message = ""
	m.detail = &detailView{
		title: "Daemon captured output (last crash)",
		lines: build(m.wrapWidth()),
		build: build,
	}
	return m, nil
}

func (m Model) viewSelected() (tea.Model, tea.Cmd) {
	switch m.tab {
	case tabAgents:
		if agents := m.visibleAgents(); m.cursor < len(agents) {
			a := agents[m.cursor]
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
			// Fetched once at open time (not on every resize rebuild).
			snapshot := m.app.SignatureSnapshot(m.ctx, r.Signature)
			build := func(width int) []string { return m.auditDetailLines(r, snapshot, width) }
			m.message = ""
			d := &detailView{
				title: fmt.Sprintf("%s #%d", kind, r.ID),
				lines: build(m.wrapWidth()),
				build: build,
			}
			// Only the Escalations detail is confirmable via enter and
			// carries the per-entry actions (c/x/l), which act on this
			// snapshotted id — never the live cursor.
			if m.tab == tabEscalations {
				d.confirmID = r.ID
				d.escRetryable = m.canRetry(r)
			}
			m.detail = d
		}
	}
	return m, nil
}

// detailPageSize is how many detail lines fit under the header and help:
// header title + tabs + blank (3), detail title + blank (2), the more-lines
// indicator (1), and blank + help (2) — plus the error line and the daemon
// health banner when present.
func (m Model) detailPageSize() int {
	if m.height <= 0 {
		return 20
	}
	chrome := 8
	if m.data.err != nil {
		chrome++
	}
	if m.data.daemonHealth.Banner() != "" {
		chrome++
	}
	return max(1, m.height-chrome)
}

// listPageSize is how many list rows fit under the current pane chrome,
// mirroring detailPageSize's accounting (AR-002): header title + tabs +
// blank (3), the more-rows indicator (1), and blank + help (2) — plus the
// error line, the daemon health banner, the search/filter lines, and the
// prompt, hint, and status areas when present.
func (m Model) listPageSize() int {
	if m.height <= 0 {
		return 20
	}
	chrome := 6
	if m.data.err != nil {
		chrome++
	}
	if m.data.daemonHealth.Banner() != "" {
		chrome++
	}
	if m.searching || (m.tab.isList() && m.query[m.tab] != "") {
		chrome++
	}
	if m.tab == tabSignatures && m.sigMode != "" {
		chrome++
	}
	if m.prompt != nil {
		chrome += 2
	}
	if m.message != "" {
		chrome += 2
	}
	if m.status != nil {
		chrome += 2
	}
	return max(1, m.height-chrome)
}

// window clamps the active tab's offset to n rows and returns the visible
// slice bounds; the caller renders rows[start:end] and the more-rows
// indicator when end < n (AR-002, AR-009).
func (m Model) window(n int) (start, end int) {
	page := m.listPageSize()
	start = min(m.offsets[m.tab], max(0, n-page))
	start = max(0, start)
	end = min(start+page, n)
	return start, end
}

// scrollCursorIntoView moves the active list tab's offset so the shared
// cursor stays visible (AR-003, AR-004).
func (m *Model) scrollCursorIntoView() {
	if !m.tab.isList() {
		return
	}
	if m.cursor < m.offsets[m.tab] {
		m.offsets[m.tab] = m.cursor
	}
	if page := m.listPageSize(); m.cursor >= m.offsets[m.tab]+page {
		m.offsets[m.tab] = m.cursor - page + 1
	}
}

// clampListViewport keeps every list tab's offset within
// [0, rowCount−pageSize] and the shared cursor within the active tab's
// visible (filtered) rows (CR-007, CR-008, CR-016).
func (m *Model) clampListViewport() {
	page := m.listPageSize()
	for _, t := range []tab{tabAgents, tabEscalations, tabAudit, tabSignatures} {
		if maxOff := max(0, m.rowCountFor(t)-page); m.offsets[t] > maxOff {
			m.offsets[t] = maxOff
		}
	}
	if m.cursor >= m.rowCount() {
		m.cursor = max(0, m.rowCount()-1)
	}
	m.scrollCursorIntoView()
}

// renderMoreRows draws the clipped-rows affordance, matching the detail
// overlay's more-lines indicator (AR-009).
func (m Model) renderMoreRows(b *strings.Builder, remaining int) {
	if remaining > 0 {
		fmt.Fprintf(b, "%s\n", m.styles().help.Render(
			fmt.Sprintf("… %d more row(s) — ↓ to scroll", remaining)))
	}
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
	// On a very tight cap the trailing minimum could swallow the whole
	// budget; primary keeps a readable floor (it is the favored field).
	if remaining-trailing < 8 {
		trailing = max(remaining-8, 0)
	}
	return remaining - trailing, trailing
}

// detailField appends a labelled, wrapped block; empty values are skipped.
func (m Model) detailField(lines []string, width int, label, value string) []string {
	if strings.TrimSpace(value) == "" {
		return lines
	}
	lines = append(lines, m.styles().section.Render(label))
	for _, ln := range wrapText(value, width) {
		lines = append(lines, "  "+ln)
	}
	return lines
}

func (m Model) agentDetailLines(a domain.AgentTransition, w int) []string {
	var lines []string
	lines = m.detailField(lines, w, "Short name", orDash(m.data.status.AgentName(a.AgentID)))
	lines = m.detailField(lines, w, "Agent id", a.AgentID)
	lines = m.detailField(lines, w, "Workspace", locationLabel(a.WorkspaceID,
		func() (string, int, bool) {
			ws, ok := m.data.status.Workspaces[a.WorkspaceID]
			return ws.Label, ws.Number, ok
		}))
	lines = m.detailField(lines, w, "Tab", locationLabel(a.TabID,
		func() (string, int, bool) {
			tab, ok := m.data.status.Tabs[a.TabID]
			return tab.Label, tab.Number, ok
		}))
	lines = m.detailField(lines, w, "Pane", a.PaneID)
	lines = m.detailField(lines, w, "Type", a.AgentType)
	lines = m.detailField(lines, w, "Status", a.Status)
	if !a.At.IsZero() {
		lines = m.detailField(lines, w, "Last transition", a.At.Format(time.RFC3339))
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

func (m Model) auditDetailLines(r domain.AuditRecord, snapshot string, w int) []string {
	var lines []string
	agent := r.AgentID
	if n := m.data.status.AgentName(r.AgentID); n != "" {
		agent = fmt.Sprintf("%s (%s)", n, r.AgentID)
	}
	lines = m.detailField(lines, w, "When", r.CreatedAt.Format(time.RFC3339))
	lines = m.detailField(lines, w, "Status", r.Status)
	lines = m.detailField(lines, w, "Situation", string(r.SituationType))
	lines = m.detailField(lines, w, "Agent", agent)
	lines = m.detailField(lines, w, "Agent type", m.agentTypeFor(r))
	lines = m.detailField(lines, w, "Confidence", fmt.Sprintf("%.2f", r.Confidence))
	lines = m.detailField(lines, w, "Trigger", r.Trigger)
	lines = m.detailField(lines, w, "Suggestion", r.Suggestion)
	lines = m.detailField(lines, w, "Action", r.Action)
	lines = m.detailField(lines, w, "Input", r.Input)
	lines = m.detailField(lines, w, "Rationale", r.Rationale)
	lines = m.detailField(lines, w, "LLM output", r.LLMOutput)
	lines = m.detailField(lines, w, "Signature", r.Signature)
	if r.Signature != "" {
		if row, ok := m.ruleFor(r.Signature); ok {
			lines = m.detailField(lines, w, "Matched rule",
				frontend.RuleSummary(row, m.data.cfg.Learning.GraduationN))
		} else {
			lines = m.detailField(lines, w, "Matched rule",
				"none yet — learned when the operator confirms or resolves this")
		}
	}
	if r.DecisionID != 0 {
		lines = m.detailField(lines, w, "Decision id", fmt.Sprintf("%d", r.DecisionID))
	}
	if r.CorrectsAuditID != 0 {
		lines = m.detailField(lines, w, "Corrects audit", fmt.Sprintf("#%d", r.CorrectsAuditID))
	}
	// Current situation: the pane content THIS record was classified from
	// (per entry). Below it, the matched rule's Original situation — the
	// signature's FIRST-seen excerpt (rule provenance), which is shared by
	// every record resolving to that rule; same semantics as the Rules
	// detail. Legacy rows predate the per-entry column and show only the
	// provenance block.
	if r.PaneExcerpt != "" {
		lines = m.detailField(lines, w, "Current situation", r.PaneExcerpt)
	}
	if r.Signature != "" {
		if snapshot != "" {
			lines = m.detailField(lines, w, "Original situation", snapshot)
		} else {
			lines = m.detailField(lines, w, "Original situation", "(not captured yet — recorded on the rule's next sighting)")
		}
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
func (m Model) signatureDetailLines(row frontend.SignatureRow, history []domain.DecisionRecord, graduationN, w int) []string {
	var lines []string
	lines = m.detailField(lines, w, "Signature", row.Signature)
	lines = m.detailField(lines, w, "Situation", string(row.SituationType))
	lines = m.detailField(lines, w, "Agent type", orDash(row.AgentType))
	lines = m.detailField(lines, w, "Mode", string(row.Mode))
	lines = m.detailField(lines, w, "Streak", fmt.Sprintf("%d/%d confirmations toward graduation", row.ConsecutiveConfirmations, graduationN))
	lines = m.detailField(lines, w, "Confidence", fmt.Sprintf("%.2f (cached)", row.CachedConfidence))
	if row.TopAction != "" {
		lines = m.detailField(lines, w, "Top action", fmt.Sprintf("%q over %d decision(s)", row.TopAction, row.Decisions))
	}
	if row.GuardState != "" {
		lines = m.detailField(lines, w, "Guard", row.GuardState)
	}
	if !row.UpdatedAt.IsZero() {
		lines = m.detailField(lines, w, "Updated", row.UpdatedAt.Format(time.RFC3339))
	}
	// Rule provenance: what the pane showed when this signature was first
	// seen — the situation the learned action answers.
	if row.PaneExcerpt != "" {
		lines = m.detailField(lines, w, "Original situation", row.PaneExcerpt)
	} else {
		lines = m.detailField(lines, w, "Original situation", "(not captured yet — recorded on the rule's next sighting)")
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
		lines = m.detailField(lines, w, "Recent decisions (newest first)", strings.TrimRight(b.String(), "\n"))
	}
	if a := row.LastAudit; a != nil {
		lines = m.detailField(lines, w, "Last audit",
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
	if item == nil {
		return m, nil
	}
	switch item.kind {
	case "indicator", "capture":
		m.message = "read-only — edit config.toml (the daemon reloads on save)"
		return m, nil
	case "field":
	default:
		return m, nil
	}
	// Free-text fields (argv templates, template strings, paths) are
	// read-only in the TUI (CR-036): the one-line prompt round-trip mangles
	// them. `hap config set` still accepts every key.
	if !frontend.FieldTUIEditable(item.key) {
		m.message = fmt.Sprintf("%s is read-only in the TUI — edit config.toml or run: hap config set %s <value>", item.key, item.key)
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
	case "indicator", "capture":
		m.message = "read-only — edit config.toml (the daemon reloads on save)"
		return m, nil
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
	st := m.styles()
	var b strings.Builder

	state := st.running.Render("● running")
	if m.data.status.Paused {
		state = st.paused.Render("■ PAUSED (kill switch)")
	}
	fmt.Fprintf(&b, "%s  %s\n", st.title.Render("Herd Auto Prompter"), state)

	var tabs []string
	for i, name := range tabNames {
		label := fmt.Sprintf(" %s ", name)
		if i == int(tabEscalations) && len(m.data.escalations) > 0 {
			label = fmt.Sprintf(" %s(%d) ", name, len(m.data.escalations))
		}
		if tab(i) == m.tab {
			tabs = append(tabs, st.activeTab.Render(label))
		} else {
			tabs = append(tabs, st.inactiveTab.Render(label))
		}
	}
	fmt.Fprintf(&b, "%s\n\n", strings.Join(tabs, "|"))

	// Daemon health banner: a hung, crash-looping, or degraded daemon otherwise
	// looks identical to "all quiet" (no escalations). Error states use the
	// error palette; degraded/stale (a working fallback) use warn.
	if banner := m.data.daemonHealth.Banner(); banner != "" {
		style := st.warn
		if m.data.daemonHealth.Severity() == frontend.DaemonError {
			style = st.err
			// The captured crash output explains the "why"; point the operator
			// at the in-app viewer (same line, no extra layout row).
			if m.data.daemonHealth.StderrLog != "" {
				banner += "  ·  press ! for captured output"
			}
		}
		fmt.Fprintf(&b, "%s\n", style.Render(banner))
	}

	if m.data.err != nil {
		fmt.Fprintf(&b, "%s\n", st.err.Render("error: "+m.data.err.Error()))
	}
	// Embedding-model drift banner: stored rule embeddings were minted by a
	// different model than the configured one, so semantic matching misses
	// until they are re-computed. Suppressed while the model file itself is
	// missing — a re-embed cannot run yet, and the semantic-matching status
	// line already reports the missing model.
	if d := m.data.status.Drift; d.Detected && !d.ModelMissing {
		fmt.Fprintf(&b, "%s\n", st.warn.Render(fmt.Sprintf(
			"⚠ embedding model changed — %d of %d rules need re-compute; press R or run: hap signatures reembed",
			d.Stale, d.Total)))
	}

	if m.detail != nil {
		m.renderDetail(&b)
		return b.String()
	}

	// Search bar / active-filter line (its height is accounted for in
	// listPageSize so the body never overflows the pane).
	if m.searching {
		fmt.Fprintf(&b, "%s%s█\n", st.section.Render("search> "), m.query[m.tab])
	} else if m.tab.isList() && m.query[m.tab] != "" {
		fmt.Fprintf(&b, "%s\n", st.help.Render(
			fmt.Sprintf("filter: %q — / to edit, backspace to clear", m.query[m.tab])))
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
	// Durable status area (CR-025): the last action outcome stays readable
	// until the next one replaces it, styled ok/error from the palette
	// (CR-026).
	if m.status != nil {
		mark, style := "✓", st.ok
		if m.status.err {
			mark, style = "✗", st.err
		}
		// Errors can embed multi-line subprocess output; the status area
		// budgets exactly one line, so flatten and truncate.
		text := oneLine(m.status.text, max(20, m.contentWidth()-12))
		fmt.Fprintf(&b, "\n%s\n", style.Render(
			fmt.Sprintf("%s %s  %s", mark, text, m.status.at.Format("15:04:05"))))
	}
	fmt.Fprintf(&b, "\n%s", st.help.Render(m.helpLine()))
	return b.String()
}

func (m Model) helpLine() string {
	if m.detail != nil {
		if m.detail.confirmID != 0 {
			retry := ""
			if m.detail.escRetryable {
				retry = "  l: retry LLM"
			}
			return "enter: confirm+send  c: correct  x: delete" + retry +
				"  ↑/↓: scroll  tab: switch tab  esc/q/v: close"
		}
		return "↑/↓: scroll  tab: switch tab  esc/q/v: close"
	}
	if m.searching {
		return "type to filter  backspace: erase  esc/enter: apply & close"
	}
	common := "tab: switch  ↑/↓: select  p: pause  r: resume  q: quit"
	if d := m.data.status.Drift; d.Detected && !d.ModelMissing {
		common = "R: re-embed  " + common
	}
	switch m.tab {
	case tabAgents:
		return "v: details  n: rename agent  /: search  " + common
	case tabEscalations:
		return "enter/y: confirm+send  c: correct  l: retry LLM  space: mark  x: delete  X: prune old  v: details  /: search  " + common
	case tabAudit:
		return "c: correct decision  v: details  /: search  " + common
	case tabSignatures:
		return "enter/v: details  x: delete  f: filter mode  /: search  " + common
	case tabConfig:
		return "enter/e: edit field  a: add pattern  t: add task source  x: remove  X: clear data  " + common
	}
	return common
}

// renderSignatures draws the learned-signature list (the Rules tab).
func (m Model) renderSignatures(b *strings.Builder) {
	st := m.styles()
	sigs := m.visibleSignatures()
	if m.sigMode != "" {
		fmt.Fprintf(b, "%s\n", st.section.Render("filter: mode="+string(m.sigMode)+"  (f cycles)"))
	}
	if len(sigs) == 0 {
		if len(m.data.signatures) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no signatures match the filter — f cycles mode, / edits search"))
			return
		}
		fmt.Fprintln(b, m.styles().help.Render("no learned signatures yet — confirm suggestions to teach hap"))
		return
	}
	gradN := m.data.cfg.Learning.GraduationN
	// The signature column sizes to the widest visible id so the full id
	// renders untruncated (CR-032); the fixed columns after it are ~48
	// cells, so the action budget shifts right with the column.
	sigW := 18
	for _, r := range sigs {
		if n := runewidth.StringWidth(r.Signature); n > sigW {
			sigW = n
		}
	}
	actWidth, _ := m.budget(sigW+48, false)
	start, end := m.window(len(sigs))
	for i := start; i < end; i++ {
		r := sigs[i]
		line := fmt.Sprintf("%-*s %-9s %-10s %-11s %d/%d conf=%.2f  %s",
			sigW, r.Signature, r.SituationType, orDash(r.AgentType), r.Mode,
			r.ConsecutiveConfirmations, gradN, r.CachedConfidence,
			oneLine(r.TopAction, actWidth))
		switch {
		case i == m.cursor:
			line = st.selected.Render(line)
		case r.Mode == domain.ModeAutonomous:
			line = st.ok.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(sigs)-end)
}

// renderDetail draws the open detail overlay in place of the tab body.
func (m Model) renderDetail(b *strings.Builder) {
	st := m.styles()
	fmt.Fprintf(b, "%s\n\n", st.title.Render(m.detail.title))
	page := m.detailPageSize()
	lines := m.detail.lines
	start := min(m.detail.offset, max(0, len(lines)-1))
	end := min(start+page, len(lines))
	for _, ln := range lines[start:end] {
		fmt.Fprintln(b, ln)
	}
	if end < len(lines) {
		fmt.Fprintf(b, "%s\n", st.help.Render(fmt.Sprintf("… %d more line(s) — ↓ to scroll", len(lines)-end)))
	}
	fmt.Fprintf(b, "\n%s", st.help.Render(m.helpLine()))
}

func (m Model) renderAgents(b *strings.Builder) {
	agents := m.visibleAgents()
	if len(agents) == 0 {
		if len(m.data.status.MonitoredAgents) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no agents match the filter — / edits, backspace clears"))
		} else {
			fmt.Fprintln(b, m.styles().help.Render("no agents detected"))
		}
		return
	}
	start, end := m.window(len(agents))
	for i := start; i < end; i++ {
		a := agents[i]
		name := orDash(m.data.status.AgentName(a.AgentID))
		line := fmt.Sprintf("%-18s %-12s %-12s %s", name, a.AgentID, a.AgentType, a.Status)
		if i == m.cursor {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(agents)-end)
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
	esc := m.visibleEscalations()
	if len(esc) == 0 {
		if len(m.data.escalations) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no escalations match the filter — / edits, backspace clears"))
		} else {
			fmt.Fprintln(b, m.styles().help.Render("no pending escalations — the herd is unblocked 🎉"))
		}
		return
	}
	// Prefix: "<mark> #%-5d %-8s %-10s %-8s agent=%-14s rule=%-6s " → 71 cells.
	const escPrefix = 71
	start, end := m.window(len(esc))
	for i := start; i < end; i++ {
		e := esc[i]
		agent := e.AgentID
		if n := m.data.status.AgentName(e.AgentID); n != "" {
			agent = n
		}
		mark := " "
		if m.marked[e.ID] {
			mark = "✓"
		}
		rWidth, sWidth := m.budget(escPrefix, e.Suggestion != "")
		line := fmt.Sprintf("%s #%-5d %-8s %-10s %-8s agent=%-14s rule=%-6s %s",
			mark, e.ID, e.CreatedAt.Format("15:04:05"), e.SituationType,
			oneLine(orDash(m.agentTypeFor(e)), 8), agent,
			m.ruleMarker(e.Signature), oneLine(e.Rationale, rWidth))
		if e.Suggestion != "" {
			line += "  → " + oneLine(e.Suggestion, sWidth)
		}
		if i == m.cursor {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(esc)-end)
}

func (m Model) renderAudit(b *strings.Builder) {
	rows := m.visibleAudit()
	if len(rows) == 0 {
		if len(m.data.audit) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no audit records match the filter — / edits, backspace clears"))
		} else {
			fmt.Fprintln(b, m.styles().help.Render("no audit records yet"))
		}
		return
	}
	// Prefix up to the action column is ~53 fixed cells + "rule=%-6s " (12).
	actWidth, _ := m.budget(65, false)
	start, end := m.window(len(rows))
	for i := start; i < end; i++ {
		r := rows[i]
		line := fmt.Sprintf("#%-5d %-14s %-9s %-10s conf=%.2f rule=%-6s %s",
			r.ID, r.CreatedAt.Format("01-02 15:04:05"), r.Status, r.SituationType,
			r.Confidence, m.ruleMarker(r.Signature), oneLine(r.Action, actWidth))
		if i == m.cursor {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(rows)-end)
}

func (m Model) renderConfig(b *strings.Builder) {
	st := m.styles()
	if len(m.items) == 0 {
		fmt.Fprintln(b, m.styles().help.Render("no configuration loaded"))
		return
	}
	lastKind := ""
	for i, item := range m.items {
		if item.kind != lastKind {
			lastKind = item.kind
			switch item.kind {
			case "field":
				fmt.Fprintln(b, st.section.Render("Config"))
			case "pattern":
				fmt.Fprintf(b, "\n%s\n", st.section.Render(fmt.Sprintf(
					"Never-auto patterns (operator; %s)", m.seedLabel())))
			case "source":
				fmt.Fprintf(b, "\n%s\n", st.section.Render("Task sources"))
			case "indicator":
				fmt.Fprintf(b, "\n%s\n", st.section.Render("Safety indicators (read-only — edit config.toml)"))
			case "capture":
				fmt.Fprintf(b, "\n%s\n", st.section.Render("Capture delays (read-only — edit config.toml)"))
			}
		}
		// Long values (argv templates, paths) truncate to one line (CR-037).
		line := "  " + oneLine(item.label, m.contentWidth()-2)
		if i == m.cursor {
			line = st.selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	if len(m.data.cfg.Safety.NeverAutoPatterns) == 0 {
		fmt.Fprintf(b, "\n%s\n", st.section.Render(fmt.Sprintf(
			"Never-auto patterns: none from operator (%s) — press a to add", m.seedLabel())))
	}
	if len(m.data.cfg.TaskSources) == 0 {
		fmt.Fprintf(b, "%s\n", st.section.Render("Task sources: none — press t to add"))
	}
}

// seedLabel names the shipped seed patterns' state: the count when active,
// or an explicit marker when safety.disable_seed dropped them (so the
// Config tab never contradicts the new editable field).
func (m Model) seedLabel() string {
	if m.data.cfg.Safety.DisableSeed {
		return "seed disabled"
	}
	return fmt.Sprintf("+%d seed active", len(domain.SeedNeverAutoPatterns))
}

func (m Model) renderKills(b *strings.Builder) {
	if len(m.data.kills) == 0 {
		fmt.Fprintln(b, m.styles().help.Render("no pause/kill events recorded"))
		return
	}
	for i, e := range m.data.kills {
		line := fmt.Sprintf("#%-4d %-20s %-8s by %s",
			e.ID, e.CreatedAt.Format(time.RFC3339), e.State, e.Author)
		if i == m.cursor {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
}

// oneLine flattens newlines and truncates to limit display CELLS (not
// runes): row budgets are in cells, and wide runes (CJK, emoji) arriving
// verbatim from pane content would otherwise overflow the row and break
// the pane-height invariant (AR-010).
func oneLine(s string, limit int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if limit < 1 {
		limit = 1
	}
	if runewidth.StringWidth(s) <= limit {
		return s
	}
	if limit == 1 {
		return "…"
	}
	return runewidth.Truncate(s, limit, "…")
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
