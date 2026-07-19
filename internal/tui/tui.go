// Package tui is the primary control surface, run as a Herdr pane. It
// mirrors every CLI capability (FR-022): monitored agents (with rename),
// pending escalations (confirm/correct), the audit log (post-hoc
// correction), the aggregated task lists of every configured task source
// (Tasks tab), learned signatures (Rules tab: inspect/filter/delete),
// configuration (Config tab: fields, never-auto patterns, task sources,
// clear-data), and the pause/kill switch with history.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	tabTasks      // aggregated checklist items of every configured task source
	tabEscalations
	tabAudit
	tabSignatures // "Rules": learned signatures (list/inspect/delete)
	tabConfig     // config fields, never-auto patterns, task sources
	tabKill
	tabCount
)

var tabNames = []string{"Agents", "Tasks", "Escalations", "Audit", "Rules", "Config", "Pause/Kill"}

// isList reports whether t renders a scrollable, searchable row list.
// Config and Pause/Kill keep their existing unwindowed navigation (AR-032).
func (t tab) isList() bool {
	return t == tabAgents || t == tabTasks || t == tabEscalations || t == tabAudit || t == tabSignatures
}

type refreshMsg struct {
	status      frontend.Status
	escalations []domain.AuditRecord
	audit       []domain.AuditRecord
	kills       []domain.KillEvent
	signatures  []frontend.SignatureRow
	tasks       []frontend.TaskGroup
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
	// pauseAction marks a result produced by the "p" pause action
	// (success or failure), so Update can clear a stale Model.pausePending
	// if the pause request itself failed (the state never transitioned, so
	// nothing will consume the flag otherwise).
	pauseAction bool
}

// openSendPromptMsg re-opens a second prompt after a LIVE escalation's
// correction text is captured, asking whether to also deliver it to the
// (blocked) agent. Chaining goes through a message because a prompt's onSubmit
// returns a tea.Cmd and cannot mutate the model directly.
type openSendPromptMsg struct {
	id     int64
	action string
}

// openAddPromptMsg re-opens a prompt after a confirm+send was refused because
// the suggested task's agent is busy, asking whether to queue the tasks to its
// declared list instead (no send). Chaining goes through a message for the same
// reason as openSendPromptMsg: the confirm command runs async and cannot open a
// prompt directly.
type openAddPromptMsg struct {
	id int64
}

// statusNote is a durable action outcome shown in the status area until the
// next mutating action starts (or a later outcome replaces it) — unlike the
// transient m.message hint line, navigation and read-only actions never clear
// it.
type statusNote struct {
	text string
	err  bool
	at   time.Time
}

type tickMsg time.Time

// clockTickMsg fires once a second to advance the live Age counter on the
// Agents tab. It only repaints — it never re-queries the store (unlike the
// slower tickMsg refresh).
type clockTickMsg time.Time

// prompt is an in-flight inline input.
type prompt struct {
	label    string
	input    string
	onSubmit func(string) tea.Cmd
	// multiline lets shift+enter (and ctrl+j, which works on terminals that
	// can't report shift+enter) insert a literal newline; the input box
	// expands one rendered line per break, and enter submits as always.
	// Pasted CR/LF line breaks are kept regardless. Only prompts whose
	// consumer understands multi-line text opt in.
	multiline bool
	// options, when non-empty, turns the prompt into a single-choice picker:
	// ↑/↓ move the highlight, enter submits the highlighted option, and typed
	// text is ignored. Used for enum-valued fields (e.g. tui.theme) so the
	// operator picks from the known set instead of typing a name blind.
	options []string
	optIdx  int
}

// promptNewlines normalizes any line-break flavor (\r\n, bare \r — common in
// terminal bracketed paste) to \n so the prompt renders one input line per
// break and the height accounting can count them.
var promptNewlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// shiftEnterSeqs are the String() forms bubbletea gives the two standard
// shift+enter escape sequences — xterm modifyOtherKeys (ESC[27;2;13~, what
// herdr transmits) and the kitty keyboard protocol (ESC[13;2u). bubbletea
// v1 has no native shift+enter key type: both arrive as an unrecognized-CSI
// message (an unexported type), so they are matched by their stable String()
// rendering instead.
var shiftEnterSeqs = map[string]bool{
	fmt.Sprintf("?CSI%+v?", []byte("27;2;13~")): true,
	fmt.Sprintf("?CSI%+v?", []byte("13;2u")):    true,
}

// isShiftEnter reports whether msg is a shift+enter keypress delivered as an
// unrecognized CSI sequence (see shiftEnterSeqs; verified against bubbletea
// v1.3.10 — re-check the String() rendering on a bubbletea upgrade). A real
// "shift+enter" KeyMsg is also accepted, so a future bubbletea that learns
// the sequence natively keeps working.
func isShiftEnter(msg tea.Msg) bool {
	if k, isKey := msg.(tea.KeyMsg); isKey {
		return k.String() == "shift+enter"
	}
	s, ok := msg.(fmt.Stringer)
	return ok && shiftEnterSeqs[s.String()]
}

// submitPrompt closes the open prompt and runs its onSubmit with the trimmed
// input; all-whitespace input cancels.
func (m Model) submitPrompt() (tea.Model, tea.Cmd) {
	p := m.prompt
	m.prompt = nil
	if len(p.options) > 0 {
		// Picker mode: submit the highlighted option verbatim.
		return m, p.onSubmit(p.options[p.optIdx])
	}
	input := strings.TrimSpace(p.input)
	if input == "" {
		m.message = "cancelled"
		return m, nil
	}
	return m, p.onSubmit(input)
}

// confirmation is a single-key Y/n guard for a quick action. Enter accepts
// the capitalized default (yes); n or Esc cancels without running anything.
type confirmation struct {
	label     string
	onConfirm func() tea.Cmd
	// clearsTaskMarks consumes the Tasks tab multi-select on ACCEPT: the
	// action's targets were captured at prompt-open and task marks are
	// positional (they renumber after a delete), so they cannot survive the
	// action — but a cancel must keep the operator's selection intact.
	clearsTaskMarks bool
	// revalidate re-checks the action's precondition against the CURRENT
	// model when the operator accepts, since a refresh can land between the
	// question and the answer. Nil skips the re-check (the action carries its
	// own staleness guard, as the task mutations' expected-text does).
	// Returning false aborts with the returned reason.
	revalidate func(Model) (string, bool)
}

// detailView is a full-record overlay opened with `v` on the Agents,
// Escalations, Audit, and Rules tabs. Scalar fields stay untruncated; captured
// pane previews may use the operator-configured tail height.
type detailView struct {
	title                string
	lines                []string                                // wrapped to the pane width at open/resize
	offset               int                                     // first visible line (↑/↓ scroll)
	build                func(width int, expanded bool) []string // rebuilds lines from the snapshot on resize/toggle
	hasExpandablePreview bool                                    // v toggles long-field previews instead of closing
	previewExpanded      bool                                    // false = title + compact field-specific tail
	// confirmID is the escalation's audit id captured at open time, so
	// enter confirms the record ON SCREEN even if a background refresh
	// clamped the list cursor. 0 = not a confirmable escalation detail.
	// The per-entry actions (c/x/l) act on this id too, never the live
	// cursor. escRetryable snapshots whether "l: retry LLM" is offered.
	confirmID    int64
	escRetryable bool
	// focusAgentID is the agent recorded on an escalation detail. Its current
	// pane coordinates are resolved from live status when `f` is pressed, so a
	// background refresh or list-cursor move cannot retarget the action.
	focusAgentID string
	// ruleDetail marks an escalation/audit overlay, and ruleSignature snapshots
	// the record's signature so `t: see rule` jumps to the rule of the record ON
	// SCREEN (same reason as confirmID/focusAgentID). The bool is what gates the
	// binding, not a non-empty signature: an over-masked record legitimately has
	// none, and must report that rather than silently no-op — while `t` on an
	// unrelated overlay (a signature or the daemon-stderr view, which also leave
	// `agent` nil) must do nothing at all.
	ruleDetail    bool
	ruleSignature string
	// agent snapshots the agent an agents-tab detail was opened for, so the
	// clock tick can rebuild its lines against the current clock (the live Age
	// would otherwise freeze at open time — the build closure captures m by
	// value). nil for non-agent details.
	agent *domain.AgentTransition
	// task snapshots the checklist item a Tasks-tab detail was opened for, so
	// the in-overlay actions (e/x/f) act on the item ON SCREEN even if a
	// background refresh moved the list cursor. nil for non-task details.
	task *taskRow
}

// ruleItem is one navigable row of the Config tab. "scoped-pattern" and
// "capture" rows are read-only (AR-034, AR-035): they render for
// visibility and refuse edit/remove with a config.toml pointer. "shortcut"
// rows run guarded one-off setup actions.
type ruleItem struct {
	kind  string // "field" | "pattern" | "source" | "scoped-pattern" | "capture" | "shortcut"
	key   string // config field key (fields)
	index int    // slice index (patterns / sources)
	value string // pattern text / source path — verified on removal
	label string // rendered row
}

// Model is the Bubble Tea model.
type Model struct {
	app *frontend.App
	ctx context.Context
	// inflight counts mutation Cmds handed to bubbletea, which does NOT wait
	// for them on quit; Run drains it so a send confirmed just before 'q'
	// still completes (and spawns its submit retries) before the process
	// exits. Pointer: Model is copied by value on every update.
	inflight *sync.WaitGroup

	tab     tab
	data    refreshMsg
	items   []ruleItem     // Config tab rows, rebuilt on refresh
	sigMode domain.Mode    // Rules tab display filter: "" = all
	marked  map[int64]bool // Escalations tab multi-select (audit ids), space toggles
	// taskMarks is the Tasks tab multi-select, keyed by taskMarkKey
	// (group index + item number). Space toggles; d/x consume the set.
	taskMarks map[string]bool
	message   string
	prompt    *prompt
	confirm   *confirmation
	detail    *detailView
	width     int
	height    int

	// installShortcut is injectable so the key flow can be tested without
	// writing /usr/local/bin. A nil value uses installHAPShortcut.
	installShortcut func() error

	// cursors is the selected row of each tab, remembered across tab switches
	// so returning to a tab restores the row you left it on (CR-038). Only the
	// active tab's entry is ever read — a background tab's row set can shift
	// under its remembered cursor, so arriveAtTab clamps on arrival.
	cursors   [tabCount]int
	offsets   [tabCount]int    // per-list viewport offset (AR-001)
	query     [tabCount]string // per-tab search filter (AR-013)
	searching bool             // search-input mode on the active tab (AR-011)
	status    *statusNote      // durable action outcome (CR-025)
	st        *styles          // palette-resolved styles; nil = default palette
	// now is the clock the live Age counter renders against, advanced by the
	// 1s clockTickMsg. Zero falls back to time.Now() (see renderNow), so tests
	// can pin it for deterministic snapshots.
	now time.Time

	// bellOut is where the terminal bell (ASCII BEL) is written; nil is a
	// safe no-op so tests never touch real IO. Run() wires it to os.Stdout.
	bellOut io.Writer
	// initialized is false until the first successful (err == nil)
	// refreshMsg has been processed. It gates all bell logic: without it,
	// the very first refresh would look like a 0-to-N transition against
	// the model's zero-valued starting state and ring for escalations or a
	// pause that already existed before the TUI even started.
	initialized bool
	// lastMaxEscalationID / lastPaused are the bell-diffing baseline from
	// the last successful refresh. Deliberately not derived from m.data,
	// since the refreshMsg handler overwrites m.data unconditionally even
	// on a failed refresh.
	lastMaxEscalationID int64
	lastPaused          bool
	// pausePending is set synchronously the instant "p" is pressed (before
	// the pause request is dispatched), and consumed by the next refreshMsg
	// that observes the false-to-true Paused transition. Setting it
	// synchronously — rather than waiting for the pause request's result —
	// matters because Bubble Tea commands run concurrently: the periodic
	// poll's refreshMsg can otherwise be processed before this instance's
	// own actionResultMsg, making a self-caused pause look externally
	// caused. Since Update processes messages one at a time in arrival
	// order, a flag set during the "p" keypress's own Update call is
	// already true for every message processed afterward, regardless of
	// which goroutine's result lands first.
	pausePending bool
}

// renderNow returns the clock the Agents tab renders Age against: the
// clock-tick time, or the wall clock when unset (fresh model / tests that
// don't drive the tick).
func (m Model) renderNow() time.Time {
	if m.now.IsZero() {
		return time.Now()
	}
	return m.now
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
		automation := "enabled"
		if m.data.status.AgentDisabled(a.AgentID) {
			automation = "disabled"
		}
		if m.matchesQuery(tabAgents, m.data.status.AgentName(a.AgentID),
			agentLocation(a, m.data.status), a.AgentID, a.AgentType, a.Status, automation) {
			out = append(out, a)
		}
	}
	return out
}

// taskRow is one flat row of the Tasks tab: a task-source group header, a
// checklist item, a per-group error line, or an empty-list note. Flat rows
// keep cursor/offset/search identical to the other list tabs.
type taskRow struct {
	text       string   // unstyled rendered content
	fields     []string // searchable values (AR-013)
	header     bool
	errRow     bool
	done       bool
	inProgress bool   // raw mark "-": started but not finished
	group      int    // index into m.data.tasks (== cfg.TaskSources index)
	item       int    // 1-based checklist item number; 0 = not an item row
	path       string // the group's checklist file
	itemText   string // the item's raw (untruncated) text; "" for non-item rows
}

// taskMarkKey identifies a checklist item for multi-select. Keyed by group
// (config entry) rather than path so duplicate-path sources mark
// independently; actions dedupe by (path, item) before mutating the file.
func taskMarkKey(group, item int) string { return fmt.Sprintf("%d#%d", group, item) }

// taskRows lays out the aggregated Tasks tab: one header per configured task
// source (annotated with the live agents it currently matches), followed by
// its checklist items in file order — or a single error/empty note.
func (m Model) taskRows() []taskRow {
	// Invert agentTaskSourceMatches: live agent names per source index, so a
	// header shows who the source currently feeds (same selector semantics as
	// the agent detail's "Task source" field).
	live := map[int][]string{}
	for _, a := range m.data.status.MonitoredAgents {
		name := m.data.status.AgentName(a.AgentID)
		if name == "" {
			name = a.AgentID
		}
		for _, idx := range m.agentTaskSourceMatches(a) {
			live[idx] = append(live[idx], name)
		}
	}
	var rows []taskRow
	for _, g := range m.data.tasks {
		sel, ws := g.Source.Agent, g.Source.Workspace
		if sel == "" {
			sel = "*"
		}
		if ws == "" {
			ws = "*"
		}
		hdr := fmt.Sprintf("#%d agent=%s ws=%s  %s", g.Index, sel, ws,
			truncatePathKeepBase(g.Source.Path, taskPathDisplayWidth))
		if names := live[g.Index]; len(names) > 0 {
			hdr += "  → " + strings.Join(names, ", ")
		}
		pending := 0
		for _, it := range g.Items {
			if !it.Done {
				pending++
			}
		}
		if g.Err == "" {
			hdr += fmt.Sprintf("  (%d pending / %d)", pending, len(g.Items))
		}
		// Fields include the rendered #N tokens so users can filter by what
		// they see, matching filterAudit. Every row is width-bounded: a wrapped
		// line would break the one-row-one-line accounting window/listPageSize
		// depend on.
		hfields := []string{fmt.Sprintf("#%d", g.Index), sel, ws, g.Source.Path,
			strings.Join(live[g.Index], " ")}
		rows = append(rows, taskRow{text: oneLine(hdr, max(20, m.contentWidth())),
			fields: hfields, header: true, group: g.Index, path: g.Source.Path})
		switch {
		case g.Err != "":
			rows = append(rows, taskRow{text: oneLine("  ✗ "+g.Err, max(20, m.contentWidth())),
				fields: append([]string{g.Err}, hfields...), errRow: true,
				group: g.Index, path: g.Source.Path})
		case len(g.Items) == 0:
			rows = append(rows, taskRow{text: "  (no tasks in this list)", fields: hfields,
				group: g.Index, path: g.Source.Path})
		default:
			for _, it := range g.Items {
				markCh := "  "
				if m.taskMarks[taskMarkKey(g.Index, it.Index)] {
					markCh = "✓ "
				}
				rows = append(rows, taskRow{
					text:       fmt.Sprintf("%s#%d [%s] %s", markCh, it.Index, it.Mark, oneLine(it.Text, max(20, m.contentWidth()-12))),
					fields:     append([]string{fmt.Sprintf("#%d", it.Index), it.Text, it.Mark}, hfields...),
					done:       it.Done,
					inProgress: it.Mark == domain.MarkInProgress,
					group:      g.Index,
					item:       it.Index,
					path:       g.Source.Path,
					itemText:   it.Text,
				})
			}
		}
	}
	return rows
}

// visibleTaskRows applies the Tasks tab search filter. Item/error rows carry
// their group's header fields, so filtering by agent/path keeps the whole
// group; a header also stays when any of its children match, so a matched
// item is never orphaned from its source context.
func (m Model) visibleTaskRows() []taskRow {
	rows := m.taskRows()
	if m.query[tabTasks] == "" {
		return rows
	}
	var out []taskRow
	for i := 0; i < len(rows); i++ {
		if !rows[i].header {
			if m.matchesQuery(tabTasks, rows[i].fields...) {
				out = append(out, rows[i])
			}
			continue
		}
		keep := m.matchesQuery(tabTasks, rows[i].fields...)
		for j := i + 1; !keep && j < len(rows) && !rows[j].header; j++ {
			keep = m.matchesQuery(tabTasks, rows[j].fields...)
		}
		if keep {
			out = append(out, rows[i])
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
	return Model{app: app, ctx: ctx, inflight: &sync.WaitGroup{}}
}

// Init starts the refresh loop.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(), clockTick())
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// clockTick drives the 1s Age repaint; it carries no data query.
func clockTick() tea.Cmd {
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg { return clockTickMsg(t) })
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
	if msg.err != nil {
		return msg
	}
	msg.tasks = frontend.TaskGroups(msg.cfg)
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
	// Read-only visibility rows (AR-034, AR-035): scoped never-auto rules and
	// capture-delay rules are structured config edited in config.toml.
	for i, r := range cfg.Safety.NeverAutoRules {
		scope := "*"
		if len(r.AgentTypes) > 0 {
			scope = strings.Join(r.AgentTypes, ",")
		}
		items = append(items, ruleItem{
			kind: "scoped-pattern", index: i, value: r.Pattern,
			label: fmt.Sprintf("never-auto-rule #%d  agent_types=%s  %s", i, scope, r.Pattern),
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
			label: fmt.Sprintf("capture-delay #%d  agent_type=%s start=%dms event=%dms",
				i, at, r.StartMs, r.EventMs),
		})
	}
	items = append(items, ruleItem{
		kind:  "shortcut",
		key:   "install-hap",
		label: "Create /usr/local/bin/hap symlink to this running binary",
	})
	return items
}

// Update handles events.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// shift+enter reaches bubbletea v1 as an unrecognized CSI message, not a
	// KeyMsg — catch it here. It only ever means "insert a newline" in a
	// multiline prompt; everywhere else it is ignored like any unknown key.
	if isShiftEnter(msg) {
		if m.prompt != nil && m.prompt.multiline {
			m.prompt.input += "\n"
		}
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.detail != nil && m.detail.build != nil {
			m.detail.lines = m.detail.build(m.wrapWidth(), m.detail.previewExpanded)
			bottom := max(0, len(m.detail.lines)-m.detailPageSize())
			m.detail.offset = min(m.detail.offset, bottom)
		}
		m.clampListViewport()
		return m, nil
	case refreshMsg:
		if msg.err == nil {
			if m.initialized && msg.cfg.TUI.TerminalBell {
				// Trigger 1: any escalation newer than the last successful
				// poll. One bell per poll cycle even if several appeared at
				// once — beeping N times for a burst is worse UX.
				if maxEscalationID(msg.escalations) > m.lastMaxEscalationID {
					m.ringBell()
				}
				// Trigger 2: pause just became active, and NOT because this
				// instance's own "p" press caused it (pausePending, set
				// synchronously at keypress time — see its doc comment).
				if !m.lastPaused && msg.status.Paused {
					if m.pausePending {
						m.pausePending = false
					} else {
						m.ringBell()
					}
				}
			}
			m.lastMaxEscalationID = maxEscalationID(msg.escalations)
			m.lastPaused = msg.status.Paused
			m.initialized = true
		}
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
		// Same for task marks: an item deleted (or renumbered away by an
		// external edit) must not leave a mark pointing at nothing.
		if len(m.taskMarks) > 0 {
			valid := map[string]bool{}
			for _, g := range msg.tasks {
				for _, it := range g.Items {
					valid[taskMarkKey(g.Index, it.Index)] = true
				}
			}
			for k := range m.taskMarks {
				if !valid[k] {
					delete(m.taskMarks, k)
				}
			}
		}
		return m, nil
	case actionResultMsg:
		if msg.pauseAction && msg.err != nil {
			// The pause request itself failed, so Paused never transitions
			// and the refreshMsg diff above will never consume the flag —
			// clear it here so it doesn't wrongly suppress some later,
			// unrelated external pause.
			m.pausePending = false
		}
		if msg.err != nil {
			m.status = &statusNote{text: msg.err.Error(), err: true, at: time.Now()}
		} else if msg.message != "" {
			m.status = &statusNote{text: msg.message, at: time.Now()}
		}
		// The status area shrinks the page by 2 — keep the cursor visible.
		m.clampListViewport()
		return m, m.refresh()
	case openSendPromptMsg:
		return m.openSendPrompt(msg.id, msg.action)
	case openAddPromptMsg:
		return m.openAddPrompt(msg.id)
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case clockTickMsg:
		// Repaint only: advance the Age clock, never re-query the store.
		m.now = time.Time(msg)
		// An open agent detail caches its lines behind a build closure that
		// captured the open-time clock; rebuild it against the new clock so
		// its live Age advances too (the list Age already recomputes on paint).
		if m.detail != nil && m.detail.agent != nil {
			a := *m.detail.agent
			build := func(width int, _ bool) []string { return m.agentDetailLines(a, width) }
			m.detail.build = build
			m.detail.lines = build(m.wrapWidth(), m.detail.previewExpanded)
		}
		return m, clockTick()
	case sigDetailMsg:
		if msg.err != nil {
			m.status = &statusNote{text: msg.err.Error(), err: true, at: time.Now()}
			return m, nil
		}
		gradN := m.data.cfg.Learning.GraduationN
		build := func(width int, expanded bool) []string {
			return m.signatureDetailLines(msg.row, msg.history, gradN, width, expanded)
		}
		d := &detailView{
			title:                fmt.Sprintf("Signature %s", shortSig(msg.row.Signature)),
			lines:                build(m.wrapWidth(), false),
			build:                build,
			hasExpandablePreview: msg.row.PaneExcerpt != "",
		}
		m.detail = d
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
			m.searching = false
			m.message = ""
			m.arriveAtTab((m.tab + 1) % tabCount)
		case "shift+tab", "left", "h":
			m.detail = nil
			m.searching = false
			m.message = ""
			m.arriveAtTab((m.tab + tabCount - 1) % tabCount)
		case "enter":
			// On an escalation's detail view, Enter confirms+sends the
			// record shown (by its snapshotted id, not the live cursor) and
			// returns to the list — no need to close and re-press.
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.confirmAuditID(id)
			}
			// On a task detail, Enter sends the snapshotted pending task to
			// its agent (the guards in sendTaskRow refuse non-pending items).
			if r := m.detail.task; r != nil {
				m.detail = nil
				return m.sendTaskRow(*r)
			}
			m.detail = nil
		case "y":
			if r := m.detail.task; r != nil {
				m.detail = nil
				return m.sendTaskRow(*r)
			}
		case "c":
			// Per-entry actions mirror the list, acting on the snapshotted
			// escalation id (confirmID), never the live cursor. A non-zero
			// confirmID is only set for pending escalations, so this is a live
			// correction (offers the "also send?" step).
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.correctByID(id, true)
			}
		case "e":
			if a := m.detail.agent; a != nil {
				m.detail = nil
				return m.enableAgent(*a)
			}
			// On a task detail, e edits the snapshotted item (the prompt
			// replaces the overlay; the expected-text guard still protects
			// against the file changing since the snapshot).
			if r := m.detail.task; r != nil {
				m.detail = nil
				return m.editTaskRowPrompt(*r)
			}
		case "x", "delete":
			if a := m.detail.agent; a != nil {
				m.detail = nil
				return m.disableAgentPrompt(*a)
			}
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.dismissByID(id)
			}
			if r := m.detail.task; r != nil {
				// clearsMarks=true even though the overlay ignores marks: the
				// delete renumbers every later item, so surviving positional
				// marks would silently retarget.
				m.detail = nil
				return m.confirmDeleteTaskTargets([]taskTarget{{
					path: canonicalTaskPath(r.path), item: r.item, done: r.done, text: r.itemText,
				}}, true)
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
			m.searching = false
			m.message = ""
			m.arriveAtTab((m.tab + 1) % tabCount)
		case "v":
			if m.detail.hasExpandablePreview && m.detail.build != nil {
				m.detail.previewExpanded = !m.detail.previewExpanded
				m.detail.lines = m.detail.build(m.wrapWidth(), m.detail.previewExpanded)
				m.detail.offset = 0
				return m, nil
			}
			m.detail = nil
		case "f":
			if m.detail.agent != nil {
				return m.focusAgent(*m.detail.agent)
			}
			if m.detail.focusAgentID != "" {
				return m.focusAgentByID(m.detail.focusAgentID)
			}
			if r := m.detail.task; r != nil {
				return m.focusTaskGroupAgent(r.group)
			}
		case "t":
			if m.detail.agent != nil {
				return m.showAgentTasks(*m.detail.agent)
			}
			// Gated on the marker, not on a non-empty signature: a record with
			// no signature must report why (as the list does), while `t` on a
			// signature/stderr overlay — which also leave `agent` nil — does
			// nothing.
			if m.detail.ruleDetail {
				return m.showRuleFor(m.detail.ruleSignature)
			}
		case "esc", "q":
			m.detail = nil
		}
		return m, nil
	}

	if m.confirm != nil {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter", "y", "Y":
			confirm := m.confirm
			m.confirm = nil
			// A refresh can land between the question and the answer, so the
			// answer is only as good as the state it is re-checked against.
			if confirm.revalidate != nil {
				if reason, ok := confirm.revalidate(m); !ok {
					m.message = reason
					return m, nil
				}
			}
			if confirm.clearsTaskMarks {
				m.taskMarks = nil
			}
			m.beginAction()
			return m, confirm.onConfirm()
		case "esc", "n", "N":
			m.confirm = nil
			m.message = "cancelled"
		}
		return m, nil
	}

	if m.prompt != nil && len(m.prompt.options) > 0 {
		// Picker mode: ↑/↓ (or vim k/j) move the highlight, enter submits it,
		// typed text is ignored (the choices are fixed).
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			return m.submitPrompt()
		case "esc":
			m.prompt = nil
			m.message = "cancelled"
			return m, nil
		case "up", "k":
			if m.prompt.optIdx > 0 {
				m.prompt.optIdx--
			}
			return m, nil
		case "down", "j":
			if m.prompt.optIdx < len(m.prompt.options)-1 {
				m.prompt.optIdx++
			}
			return m, nil
		default:
			return m, nil
		}
	}

	if m.prompt != nil {
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEnter:
			return m.submitPrompt()
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
		case tea.KeyCtrlJ:
			if m.prompt.multiline {
				m.prompt.input += "\n"
			}
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
		m.message = ""
		m.arriveAtTab((m.tab + 1) % tabCount)
	case "shift+tab", "left", "h":
		m.message = ""
		m.arriveAtTab((m.tab + tabCount - 1) % tabCount)
	case "up", "k":
		if m.cursors[m.tab] > 0 {
			m.cursors[m.tab]--
		}
		m.scrollCursorIntoView()
		m.message = ""
	case "down", "j":
		if m.cursors[m.tab] < m.rowCount()-1 {
			m.cursors[m.tab]++
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
		m.beginAction()
		// Set synchronously (before the request is dispatched) — see
		// Model.pausePending's doc comment for why this matters.
		m.pausePending = true
		return m, m.pauseCmd()
	case "r":
		m.beginAction()
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
			m.beginAction()
			return m, m.do("re-compute requested — daemon is re-embedding in the background",
				func(ctx context.Context) error { return m.app.RequestReembed(ctx) })
		}
		return m, nil
	case "enter":
		switch m.tab {
		case tabTasks:
			return m.sendSelectedTask()
		case tabEscalations:
			return m.confirmSelected()
		case tabSignatures:
			return m.viewSignatureDetail()
		case tabConfig:
			return m.activateSelectedConfig()
		}
	case "y":
		switch m.tab {
		case tabEscalations:
			return m.confirmSelected()
		case tabTasks:
			return m.sendSelectedTask()
		}
	case "e":
		switch m.tab {
		case tabAgents:
			return m.enableSelectedAgent()
		case tabConfig:
			return m.editSelectedRule()
		case tabTasks:
			return m.editTaskPrompt()
		}
	case "d":
		if m.tab == tabTasks {
			return m.toggleTasksDone()
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
		switch m.tab {
		case tabSignatures:
			switch m.sigMode {
			case "":
				m.sigMode = domain.ModeShadow
			case domain.ModeShadow:
				m.sigMode = domain.ModeAutonomous
			default:
				m.sigMode = ""
			}
			m.cursors[m.tab] = 0
			m.offsets[tabSignatures] = 0
			m.message = "filter: " + orDash(string(m.sigMode))
		case tabAgents:
			return m.focusSelected()
		case tabEscalations:
			return m.focusSelectedEscalation()
		case tabTasks:
			return m.focusSelectedTaskAgent()
		}
	case "a":
		switch m.tab {
		case tabConfig:
			return m.addPatternPrompt()
		case tabTasks:
			return m.addTaskPrompt()
		}
	case "t":
		switch m.tab {
		case tabConfig:
			return m.addTaskSourcePrompt()
		case tabAgents:
			return m.showSelectedAgentTasks()
		case tabEscalations, tabAudit:
			return m.showSelectedRule()
		}
	case " ":
		switch m.tab {
		case tabEscalations:
			return m.toggleMarkSelected()
		case tabTasks:
			return m.toggleTaskMarkSelected()
		}
	case "x", "delete":
		switch m.tab {
		case tabAgents:
			return m.disableSelectedAgentPrompt()
		case tabEscalations:
			return m.deleteEscalations()
		case tabTasks:
			return m.deleteTasksPrompt()
		case tabSignatures:
			return m.deleteSignaturePrompt()
		case tabConfig:
			return m.removeSelectedRule()
		case tabAudit:
			m.message = "audit log is append-only — entries can't be deleted individually"
			m.scrollCursorIntoView() // the hint line shrinks the page
		}
	case "0":
		if m.tab == tabSignatures {
			return m.resetGraduationPrompt()
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

// beginAction clears the previous durable outcome as soon as a new mutation
// starts. The new action's result will populate the status area when it
// completes; navigation and read-only actions deliberately do not call this.
func (m *Model) beginAction() {
	m.status = nil
	m.message = ""
	m.clampListViewport()
}

// do runs a mutation and reports its outcome. The inflight Add happens here,
// on the update loop — before Program.Run can return — so Run's drain never
// races the counter from zero (bubbletea always launches a returned Cmd, so
// the paired Done is guaranteed).
func (m Model) do(okMsg string, fn func(context.Context) error) tea.Cmd {
	ctx, wg := m.ctx, m.inflight
	if wg != nil {
		wg.Add(1)
	}
	return func() tea.Msg {
		if wg != nil {
			defer wg.Done()
		}
		if err := fn(ctx); err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{message: okMsg}
	}
}

// maxEscalationID returns the highest AuditRecord.ID among rows (0 if
// empty). audit_log ids are assigned by SQLite's autoincrement PK and never
// reused, so any pending escalation with an id greater than a previously
// observed max is unambiguously new.
func maxEscalationID(rows []domain.AuditRecord) int64 {
	var highest int64
	for _, r := range rows {
		if r.ID > highest {
			highest = r.ID
		}
	}
	return highest
}

// ringBell emits a single ASCII BEL (0x07). A nil bellOut (the default in
// tests and unless Run() wires it) makes this a safe no-op.
//
// This writes directly to bellOut (os.Stdout in Run()) rather than through
// Bubble Tea's own output helpers: tea.Println/tea.Printf are silently
// dropped whenever the alt screen is active (see bubbletea's
// standardRenderer's printLineMessage handling) — verified against the
// vendored source — and this TUI always runs with tea.WithAltScreen(), so
// those helpers would make the whole feature a no-op. The renderer's frame
// flush writes to the same fd from its own goroutine, but a lone BEL is a
// single byte — a single Write() of one byte cannot be torn by a
// concurrent Write() of another buffer, so the worst case is a one-frame-
// late beep, never output corruption.
func (m Model) ringBell() {
	if m.bellOut == nil {
		return
	}
	_, _ = m.bellOut.Write([]byte{0x07})
}

// pauseCmd activates the pause/kill switch, tagging its result as
// pauseAction so Update can clear Model.pausePending if the request itself
// failed — the generic do() helper has no channel for that extra signal.
func (m Model) pauseCmd() tea.Cmd {
	app, ctx := m.app, m.ctx
	return func() tea.Msg {
		if err := app.Pause(ctx); err != nil {
			return actionResultMsg{err: err, pauseAction: true}
		}
		return actionResultMsg{message: "automation paused", pauseAction: true}
	}
}

// rowCountFor counts tab t's currently visible rows: search-filter-aware
// for the four list tabs (CR-008); Config and Pause/Kill keep their raw
// counts so their navigation is untouched (AR-032).
func (m Model) rowCountFor(t tab) int {
	switch t {
	case tabAgents:
		return len(m.visibleAgents())
	case tabTasks:
		return len(m.visibleTaskRows())
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
		if esc := m.visibleEscalations(); m.cursors[m.tab] < len(esc) {
			return &esc[m.cursors[m.tab]]
		}
	case tabAudit:
		if rows := m.visibleAudit(); m.cursors[m.tab] < len(rows) {
			return &rows[m.cursors[m.tab]]
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

// confirmAuditID confirms+sends a specific escalation by id (used by the list
// and by the detail overlay, which confirms the record it snapshotted). When the
// send is refused because the suggested task's agent has started working, the
// task is still valid but delivering it now would interrupt the agent — so it
// chains to an "add to the task list instead?" prompt (openAddPromptMsg) rather
// than surfacing the error. Any other failure surfaces as-is.
func (m Model) confirmAuditID(id int64) (tea.Model, tea.Cmd) {
	app, ctx, wg := m.app, m.ctx, m.inflight
	m.beginAction()
	if wg != nil {
		wg.Add(1)
	}
	return m, func() tea.Msg {
		if wg != nil {
			defer wg.Done()
		}
		err := app.Confirm(ctx, id, true)
		if err == nil {
			return actionResultMsg{message: fmt.Sprintf("confirmed #%d and sent", id)}
		}
		if errors.Is(err, frontend.ErrSuggestionStaleAgentBusy) {
			return openAddPromptMsg{id: id}
		}
		return actionResultMsg{err: err}
	}
}

// openAddPrompt asks whether to queue a stale generated-task suggestion onto the
// agent's declared task list without sending — the agent is busy, so a send
// would interrupt it, but the task itself is still valid. Answering "y" accepts
// the escalation: it appends the tasks, resolves the escalation, and records the
// acceptance to Audits (the daemon delivers the first task on the next idle).
//
// The prompt is deliberately left EMPTY (not pre-filled "y"): the input box
// APPENDS keystrokes, so a pre-filled "y" would turn a typed "n" into "yn" —
// still HasPrefix "y" — making the decline unreachable. With no default, a bare
// Enter submits nothing → submitPrompt cancels (leaves the escalation pending,
// the safe outcome), and only an explicit "y" queues. Hence the "[y/N]" hint.
func (m Model) openAddPrompt(id int64) (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: fmt.Sprintf("agent is busy — add the tasks to its task list instead? [y/N] (#%d)", id),
		onSubmit: func(input string) tea.Cmd {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(input)), "y") {
				return func() tea.Msg {
					return actionResultMsg{message: fmt.Sprintf("#%d left pending — not added", id)}
				}
			}
			return func() tea.Msg {
				if err := app.Confirm(ctx, id, false); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("added #%d to task list (not sent)", id)}
			}
		},
	}
	return m, nil
}

func (m Model) correctSelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	return m.correctByID(rec.ID, rec.Status == "escalated")
}

// correctByID opens the correction prompt for a specific audit id — used by
// the list and by the detail overlay (which corrects its snapshotted record,
// not the live cursor). live reports whether the record is a pending
// escalation (agent waiting): for those, capturing the action chains a second
// "also send?" prompt so the corrected reply can be delivered; for a
// historical record (e.g. correcting a past auto decision) the correction is
// recorded only, never sent.
func (m Model) correctByID(id int64, live bool) (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.prompt = &prompt{
		label: fmt.Sprintf("correct #%d — action to record", id),
		onSubmit: func(input string) tea.Cmd {
			if live {
				// Defer recording to the send prompt so exactly one correction
				// is written with the chosen send flag.
				return func() tea.Msg { return openSendPromptMsg{id: id, action: input} }
			}
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

// openSendPrompt is the second step of correcting a live escalation: it asks
// whether to deliver the corrected action to the blocked agent, then records
// the correction once with the chosen send flag. It defaults to "n" (record
// only) so a bare Enter never sends unintentionally.
func (m Model) openSendPrompt(id int64, action string) (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.message = ""
	m.prompt = &prompt{
		label: fmt.Sprintf("send corrected action to the agent now? [y/N] (#%d)", id),
		input: "n",
		onSubmit: func(input string) tea.Cmd {
			send := strings.HasPrefix(strings.ToLower(strings.TrimSpace(input)), "y")
			return func() tea.Msg {
				if err := app.Resolve(ctx, id, action, send); err != nil {
					return actionResultMsg{err: err}
				}
				if send {
					return actionResultMsg{message: fmt.Sprintf("correction recorded and sent for #%d", id)}
				}
				return actionResultMsg{message: fmt.Sprintf("correction recorded for #%d (not sent)", id)}
			}
		},
	}
	return m, nil
}

// dismissByID dismisses one escalation by id — used by the detail overlay.
// The list uses deleteEscalations for its marked/cursor batch semantics.
func (m Model) dismissByID(id int64) (tea.Model, tea.Cmd) {
	m.beginAction()
	return m, m.do(fmt.Sprintf("dismissed #%d", id), func(ctx context.Context) error {
		return m.app.Dismiss(ctx, id)
	})
}

// retryByID re-invokes the LLM on one escalation by id (list and detail).
func (m Model) retryByID(id int64) (tea.Model, tea.Cmd) {
	m.beginAction()
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
	if m.cursors[m.tab] < m.rowCount()-1 {
		m.cursors[m.tab]++
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
	m.beginAction()
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
	m.beginAction()
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

// --- Tasks tab CRUD (mirrors `hap task` add/edit/done/undone/delete) ---

// selectedTaskRow returns the Tasks row under the cursor (header, item, or
// error/note row), nil when the tab is empty or the cursor is out of range.
func (m Model) selectedTaskRow() *taskRow {
	rows := m.visibleTaskRows()
	if m.cursors[m.tab] >= len(rows) {
		return nil
	}
	return &rows[m.cursors[m.tab]]
}

// taskTarget is one concrete checklist item an action applies to, captured at
// keypress time so an async command never re-resolves against a moved cursor
// or a refreshed snapshot. text is passed to the App mutation as its
// expected-text guard: task numbers are positional, so if the file changes
// while a prompt/confirm is open the mutation aborts instead of silently
// hitting a renumbered line.
type taskTarget struct {
	path string
	item int
	done bool
	text string
}

// truncatePathKeepBase shortens a long path for one-line display, always
// preserving the final path element: "…/<tail>" keeps as many trailing
// directories as fit within limit display cells. The full path stays in the
// row's search fields and the detail view.
func truncatePathKeepBase(p string, limit int) string {
	if runewidth.StringWidth(p) <= limit {
		return p
	}
	base := filepath.Base(p)
	out := "…/" + base
	dirs := strings.Split(strings.Trim(strings.TrimSuffix(p, base), "/"), "/")
	for i := len(dirs) - 1; i >= 0; i-- {
		cand := "…/" + strings.Join(dirs[i:], "/") + "/" + base
		if runewidth.StringWidth(cand) > limit {
			break
		}
		out = cand
	}
	// A basename longer than the limit still gets bounded (tail-truncated).
	return oneLine(out, limit)
}

// taskPathDisplayWidth is the header/prompt budget for a task source path —
// wide enough to keep distinguishing directories, short enough that the
// pending count and live-agent names survive on ordinary pane widths.
const taskPathDisplayWidth = 44

// canonicalTaskPath normalizes a source path for identity comparisons (the
// duplicate dedupe and the per-file delete ordering): absolute + symlinks
// resolved, best-effort. Two config spellings of one file (relative vs
// absolute, /var vs /private/var) must not slip past the dedupe and mutate
// the same line twice.
func canonicalTaskPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

// markedTaskTargets returns the marked items (in list order) or, with no
// marks, the item under the cursor. Duplicate-path sources can mark the same
// physical item twice; targets dedupe by canonical (path, item) so a bulk
// action never mutates one file line twice.
func (m Model) markedTaskTargets() []taskTarget {
	var targets []taskTarget
	seen := map[string]bool{}
	for _, g := range m.data.tasks {
		for _, it := range g.Items {
			if !m.taskMarks[taskMarkKey(g.Index, it.Index)] {
				continue
			}
			p := canonicalTaskPath(g.Source.Path)
			key := p + "\x00" + strconv.Itoa(it.Index)
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, taskTarget{path: p, item: it.Index, done: it.Done, text: it.Text})
		}
	}
	if len(targets) == 0 {
		if r := m.selectedTaskRow(); r != nil && r.item > 0 {
			targets = append(targets, taskTarget{
				path: canonicalTaskPath(r.path), item: r.item, done: r.done, text: r.itemText})
		}
	}
	return targets
}

// describeTasks names the action targets compactly: "task #3" or
// "3 tasks (#1 #2 #5)", eliding a long list.
func describeTasks(targets []taskTarget) string {
	if len(targets) == 1 {
		return fmt.Sprintf("task #%d", targets[0].item)
	}
	var parts []string
	for i, tg := range targets {
		if i == 6 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("#%d", tg.item))
	}
	return fmt.Sprintf("%d tasks (%s)", len(targets), strings.Join(parts, " "))
}

// toggleTaskMarkSelected flips the multi-select mark on the checklist item
// under the cursor and advances, so repeated space marks a run of rows.
func (m Model) toggleTaskMarkSelected() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil || r.item == 0 {
		m.message = "space marks checklist items — move the cursor onto a task"
		return m, nil
	}
	if m.taskMarks == nil {
		m.taskMarks = map[string]bool{}
	}
	key := taskMarkKey(r.group, r.item)
	if m.taskMarks[key] {
		delete(m.taskMarks, key)
	} else {
		m.taskMarks[key] = true
	}
	if len(m.taskMarks) == 0 {
		m.message = "no marks — d/x act on the row under the cursor"
	} else {
		m.message = fmt.Sprintf("%d marked — d toggles done, x deletes", len(m.taskMarks))
	}
	if m.cursors[m.tab] < m.rowCount()-1 {
		m.cursors[m.tab]++
	}
	m.scrollCursorIntoView()
	return m, nil
}

// toggleTasksDone flips done/pending on the marked items (or the cursor row),
// each item individually — mirroring `hap task done`/`undone`. Toggling never
// renumbers, so per-item failures skip and continue safely.
func (m Model) toggleTasksDone() (tea.Model, tea.Cmd) {
	targets := m.markedTaskTargets()
	if len(targets) == 0 {
		m.message = "d toggles done — move the cursor onto a task or mark with space"
		return m, nil
	}
	app, desc := m.app, describeTasks(targets)
	m.taskMarks = nil // the action consumes the selection
	m.beginAction()
	return m, func() tea.Msg {
		toggled := 0
		var skipped []string
		var firstErr error
		for _, tg := range targets {
			if _, err := app.SetTaskDone("", tg.path, tg.item, !tg.done, tg.text); err != nil {
				skipped = append(skipped, fmt.Sprintf("#%d", tg.item))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			toggled++
		}
		if firstErr != nil {
			return actionResultMsg{err: fmt.Errorf("toggled %d, skipped %s: %w",
				toggled, strings.Join(skipped, " "), firstErr)}
		}
		return actionResultMsg{message: fmt.Sprintf("toggled %s", desc)}
	}
}

// deleteTasksPrompt confirms then deletes the marked items (or the cursor
// row). Unlike dismissing an escalation, this destroys checklist lines the
// operator wrote, so it gets the y/n guard.
func (m Model) deleteTasksPrompt() (tea.Model, tea.Cmd) {
	targets := m.markedTaskTargets()
	if len(targets) == 0 {
		// Nothing marked and the cursor is on a source header: x retires the
		// whole source instead. Marks keep winning (markedTaskTargets is
		// consulted first), so this never shadows a bulk item delete.
		if r := m.selectedTaskRow(); r != nil && r.header {
			return m.removeTaskSourcePrompt(r.group)
		}
		m.message = "x deletes a task — move the cursor onto one or mark with space"
		return m, nil
	}
	return m.confirmDeleteTaskTargets(targets, true)
}

// taskSourceRemovable reports whether a task source may be retired from the
// Tasks tab, and if not, why. A source is removable only once it cannot be
// serving anyone: no live agent matches its selectors, or every task in it is
// genuinely finished.
//
// Both "unknown" inputs fail closed, because neither is evidence of safety:
// an agent list herdr would not answer is not an empty herd, and a checklist
// that would not read is not an empty checklist. Either can still hide live
// work. The unguarded Config-tab `x` and `hap task-source remove` remain the
// force path for an entry this refuses.
func (m Model) taskSourceRemovable(g frontend.TaskGroup) (string, bool) {
	// UnfinishedTasks, not PendingTasks: an agent mid-task has "[-]" items,
	// which read as Done and would make a live source look finished. A
	// finished list is removable whoever it feeds, so this needs no agent.
	if g.Err == "" && frontend.UnfinishedTasks([]frontend.TaskGroup{g}) == 0 {
		return "", true
	}
	if !m.data.status.AgentsKnown {
		return fmt.Sprintf("task source #%d: herdr can't say which agent it feeds — "+
			"retry, or remove the entry on the Config tab (x)", g.Index), false
	}
	// Nothing matches the selectors, so the source feeds nobody — retirable
	// whatever its file does or doesn't say. This is the case that keeps a
	// broken entry (unreadable path, dead agent) cleanable from this tab.
	agent := m.taskGroupAgent(g.Index)
	if agent == nil {
		return "", true
	}
	name := m.data.status.AgentName(agent.AgentID)
	if name == "" {
		name = agent.AgentID
	}
	if g.Err != "" {
		return fmt.Sprintf("task source #%d feeds %s but its checklist can't be read, so its "+
			"remaining work is unknown — fix the path, or remove the entry on the Config tab (x)",
			g.Index, name), false
	}
	return fmt.Sprintf("task source #%d still feeds %s and has %d unfinished task(s) — "+
		"finish them, or remove the entry on the Config tab (x)", g.Index, name,
		frontend.UnfinishedTasks([]frontend.TaskGroup{g})), false
}

// removeTaskSourcePrompt confirms, then removes the task source's config
// entry. The checklist file itself is deliberately left on disk: sources are
// often hand-written docs hap did not create and could not restore, and
// re-adding the source brings the list back untouched.
func (m Model) removeTaskSourcePrompt(group int) (tea.Model, tea.Cmd) {
	if group < 0 || group >= len(m.data.tasks) {
		return m, nil
	}
	g, app := m.data.tasks[group], m.app
	if reason, ok := m.taskSourceRemovable(g); !ok {
		m.message = reason
		return m, nil
	}
	// g.Index is the config index and g.Source.Path the raw (untruncated)
	// path the header only displays abbreviated — RemoveTaskSource re-checks
	// both, so a config that shifted underneath aborts instead of removing a
	// neighbour.
	remove := m.do(fmt.Sprintf("task source #%d removed (checklist file kept)", g.Index),
		func(c context.Context) error {
			return app.RemoveTaskSource(c, g.Index, g.Source.Path)
		})
	// A source with no path configured has no file name to name it by.
	name := filepath.Base(g.Source.Path)
	if g.Source.Path == "" {
		name = "no path configured"
	}
	m.confirm = &confirmation{
		label: fmt.Sprintf("remove task source #%d (%s)? its checklist file is kept",
			g.Index, name),
		// Removing an entry shifts every later config index down one, so a
		// positional group#item mark would silently retarget a different
		// source. Unreachable while this path requires an empty mark set, but
		// wrong to leave true by accident.
		clearsTaskMarks: true,
		// Removability was true when the question was asked; the 2s poll can
		// land before the answer. A finished list is exactly the state that
		// makes a source removable AND triggers the daemon to regenerate
		// tasks into it, so re-check rather than retire a source that just
		// picked up work (or an agent).
		revalidate: func(cur Model) (string, bool) {
			for _, now := range cur.data.tasks {
				if now.Index == g.Index && now.Source.Path == g.Source.Path {
					return cur.taskSourceRemovable(now)
				}
			}
			return fmt.Sprintf("task source #%d changed since it was listed — re-check and retry",
				g.Index), false
		},
		onConfirm: func() tea.Cmd { return remove },
	}
	return m, nil
}

// confirmDeleteTaskTargets is the shared delete flow behind the list `x`
// (marked/cursor targets) and the detail overlay `x` (the single snapshotted
// item). Every delete renumbers, so both paths consume the positional marks
// on accept; clearsMarks stays a parameter only for a future non-renumbering
// caller.
func (m Model) confirmDeleteTaskTargets(targets []taskTarget, clearsMarks bool) (tea.Model, tea.Cmd) {
	app, desc := m.app, describeTasks(targets) // name them in list order
	// Delete bottom-up per file: removing a line renumbers everything after
	// it, so descending item order keeps the remaining targets valid.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].path != targets[j].path {
			return targets[i].path < targets[j].path
		}
		return targets[i].item > targets[j].item
	})
	m.confirm = &confirmation{
		label:           fmt.Sprintf("delete %s?", desc),
		clearsTaskMarks: clearsMarks, // accept consumes the selection; cancel keeps it
		onConfirm: func() tea.Cmd {
			return func() tea.Msg {
				deleted := 0
				for _, tg := range targets {
					if _, err := app.DeleteTask("", tg.path, tg.item, tg.text); err != nil {
						// Stop, don't skip: a failure here usually means the
						// file changed under us, and later (lower) indices
						// may already point at different lines.
						return actionResultMsg{err: fmt.Errorf("deleted %d of %d: %w",
							deleted, len(targets), err)}
					}
					deleted++
				}
				return actionResultMsg{message: fmt.Sprintf("deleted %s", desc)}
			}
		},
	}
	return m, nil
}

// addTaskPrompt appends a task to the checklist of the source under the
// cursor (any of its rows — header included — names the target file).
func (m Model) addTaskPrompt() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil {
		m.message = "no task source — add one on the Config tab first (t)"
		return m, nil
	}
	if r.path == "" {
		m.message = "this source has no path configured — edit config.toml"
		return m, nil
	}
	app, path := m.app, r.path
	m.beginAction()
	m.prompt = &prompt{
		label: fmt.Sprintf("new task(s) for %s — enter: add, shift+enter: new line, esc: cancel",
			truncatePathKeepBase(path, taskPathDisplayWidth)),
		multiline: true,
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				_, n, err := app.AddTask("", path, input)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("added task #%d", n)}
			}
		},
	}
	return m, nil
}

// editTaskPrompt rewrites the text of the single item under the cursor,
// pre-filled with the current text. Deliberately not a bulk action — the
// same replacement text on many items is never what the operator wants.
func (m Model) editTaskPrompt() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil || r.item == 0 {
		m.message = "e edits a task — move the cursor onto one"
		return m, nil
	}
	return m.editTaskRowPrompt(*r)
}

// editTaskRowPrompt opens the edit prompt for a snapshotted item row — the
// shared core behind the list `e` and the detail overlay `e`. Stored literal
// `\n` sequences pre-fill as real line breaks (the box expands) and are
// re-encoded on save, so the item stays one physical checklist line.
func (m Model) editTaskRowPrompt(r taskRow) (tea.Model, tea.Cmd) {
	app, path, idx, stored := m.app, r.path, r.item, r.itemText
	m.beginAction()
	m.prompt = &prompt{
		label:     fmt.Sprintf("edit task #%d — enter: save, shift+enter: new line, esc: cancel", idx),
		input:     domain.DecodeTaskNewlines(stored),
		multiline: true,
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if _, err := app.EditTask("", path, idx, input, stored); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("task #%d updated", idx)}
			}
		},
	}
	return m, nil
}

// focusSelectedTaskAgent jumps to the live agent this task source currently
// feeds (the header's "→ name" annotation), reusing the selector match.
func (m Model) focusSelectedTaskAgent() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil {
		return m, nil
	}
	return m.focusTaskGroupAgent(r.group)
}

// focusTaskGroupAgent focuses the first live agent whose selectors match the
// given task source (config index) — shared by the list and detail `f`.
func (m Model) focusTaskGroupAgent(group int) (tea.Model, tea.Cmd) {
	for _, a := range m.data.status.MonitoredAgents {
		for _, idx := range m.agentTaskSourceMatches(a) {
			if idx == group {
				return m.focusAgent(a)
			}
		}
	}
	m.message = "no live agent matches this task source"
	return m, nil
}

// taskGroupAgent resolves the first live agent whose selectors match the
// given task source, mirroring the header's "→ name" annotation.
func (m Model) taskGroupAgent(group int) *domain.AgentTransition {
	for _, a := range m.data.status.MonitoredAgents {
		for _, idx := range m.agentTaskSourceMatches(a) {
			if idx == group {
				return &a
			}
		}
	}
	return nil
}

// sendSelectedTask delivers the pending task under the cursor to the live
// agent its source feeds (enter/y on the Tasks tab).
func (m Model) sendSelectedTask() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil || r.item == 0 {
		return m, nil
	}
	return m.sendTaskRow(*r)
}

// sendTaskRow is the shared enter/y send behind the list and the detail
// overlay: it renders the snapshotted item through the source's next-task
// template and delivers it to the matched live agent's pane. Only a truly
// pending "[ ]" item qualifies — done and in-progress items are refused, as
// is an agent that is not cleanly idle (sending into a working or blocked
// agent would splice into its stream mid-flight; the daemon's declared-task
// flow has the same idle-only rule).
func (m Model) sendTaskRow(r taskRow) (tea.Model, tea.Cmd) {
	if r.done || r.inProgress {
		m.message = "only a pending [ ] task can be sent — this one is done or in progress"
		return m, nil
	}
	agent := m.taskGroupAgent(r.group)
	if agent == nil {
		m.message = "no live agent matches this task source"
		return m, nil
	}
	if domain.AgentBusy(agent.Status) {
		m.message = fmt.Sprintf("agent %s is %s — a task can only be sent to a cleanly idle agent",
			m.data.status.AgentName(agent.AgentID), agent.Status)
		return m, nil
	}
	name := m.data.status.AgentName(agent.AgentID)
	if name == "" {
		name = agent.AgentID
	}
	// The template comes from the live config, so make sure it still belongs
	// to the snapshotted file: a task-source change while a detail overlay
	// was open must not pair one source's text with another's template.
	if r.group >= len(m.data.tasks) || m.data.tasks[r.group].Source.Path != r.path {
		m.message = "task sources changed — refresh and retry"
		return m, nil
	}
	template := m.data.tasks[r.group].Source.NextTaskTemplate
	app := m.app
	paneID, agentType, path, text, item := agent.PaneID, agent.AgentType, canonicalTaskPath(r.path), r.itemText, r.item
	send := m.do(fmt.Sprintf("task #%d sent to %s and marked [-] in progress", item, name),
		func(c context.Context) error {
			return app.SendTaskToAgent(c, paneID, agentType, name, path, template, item, text)
		})
	m.confirm = &confirmation{
		label:     fmt.Sprintf("send task #%d to %s?", item, name),
		onConfirm: func() tea.Cmd { return send },
	}
	return m, nil
}

// taskDetailLines renders the full, untruncated record of one checklist item
// for the detail overlay: status, complete text, and its source's identity.
func (m Model) taskDetailLines(r taskRow, width int) []string {
	w := max(20, width)
	status := "pending"
	switch {
	case r.inProgress:
		status = "in progress [-]"
	case r.done:
		status = "done [x]"
	}
	var lines []string
	lines = m.detailField(lines, w, "Task", fmt.Sprintf("#%d", r.item))
	lines = m.detailField(lines, w, "Status", status)
	// Stored literal `\n` sequences render as real line breaks here — the
	// detail shows the task as the agent will receive it.
	lines = m.detailField(lines, w, "Text", domain.DecodeTaskNewlines(r.itemText))
	lines = m.detailField(lines, w, "Source file", r.path)
	if r.group < len(m.data.tasks) {
		src := m.data.tasks[r.group].Source
		lines = m.detailField(lines, w, "Agent selector", orDash(src.Agent))
		lines = m.detailField(lines, w, "Workspace", orDash(src.Workspace))
	}
	var live []string
	for _, a := range m.data.status.MonitoredAgents {
		for _, idx := range m.agentTaskSourceMatches(a) {
			if idx == r.group {
				name := m.data.status.AgentName(a.AgentID)
				if name == "" {
					name = a.AgentID
				}
				live = append(live, name)
			}
		}
	}
	lines = m.detailField(lines, w, "Live agents", orDash(strings.Join(live, ", ")))
	return lines
}

// --- Agent rename ---

func (m Model) renameSelected() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	agent := agents[m.cursors[m.tab]]
	current := m.data.status.AgentName(agent.AgentID)
	target := agent.AgentID
	app, ctx := m.app, m.ctx
	m.beginAction()
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

func (m Model) disableSelectedAgentPrompt() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	return m.disableAgentPrompt(agents[m.cursors[m.tab]])
}

func (m Model) disableAgentPrompt(agent domain.AgentTransition) (tea.Model, tea.Cmd) {
	if m.data.status.AgentDisabled(agent.AgentID) {
		m.message = "agent is already disabled"
		return m, nil
	}
	app, agentID := m.app, agent.AgentID
	name := orDash(m.data.status.AgentName(agentID))
	m.confirm = &confirmation{
		label: fmt.Sprintf("disable agent %s (%s)? [Y/n]", name, agentID),
		onConfirm: func() tea.Cmd {
			return m.do(fmt.Sprintf("agent %s disabled", name), func(ctx context.Context) error {
				return app.SetAgentDisabled(ctx, agentID, true)
			})
		},
		revalidate: func(current Model) (string, bool) {
			if current.data.status.AgentDisabled(agentID) {
				return "agent is already disabled", false
			}
			return "", true
		},
	}
	return m, nil
}

func (m Model) enableSelectedAgent() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	return m.enableAgent(agents[m.cursors[m.tab]])
}

func (m Model) enableAgent(agent domain.AgentTransition) (tea.Model, tea.Cmd) {
	if !m.data.status.AgentDisabled(agent.AgentID) {
		m.message = "agent is already enabled"
		return m, nil
	}
	name := orDash(m.data.status.AgentName(agent.AgentID))
	m.beginAction()
	return m, m.do(fmt.Sprintf("agent %s enabled", name), func(ctx context.Context) error {
		return m.app.SetAgentDisabled(ctx, agent.AgentID, false)
	})
}

// focusAgent asks herdr to jump to the agent's exact pane (tab focus + zoom).
func (m Model) focusAgent(a domain.AgentTransition) (tea.Model, tea.Cmd) {
	if a.TabID == "" || a.PaneID == "" {
		m.message = "no location known for this agent"
		return m, nil
	}
	m.beginAction()
	app, tabID, paneID := m.app, a.TabID, a.PaneID
	return m, m.do("focused agent in herdr", func(ctx context.Context) error {
		return app.FocusAgent(ctx, tabID, paneID)
	})
}

func (m Model) focusSelected() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	return m.focusAgent(agents[m.cursors[m.tab]])
}

// focusAgentByID resolves an audit record's stable agent id to its current
// herdr location. Audit rows intentionally do not duplicate pane coordinates,
// which may change while the TUI is open.
func (m Model) focusAgentByID(agentID string) (tea.Model, tea.Cmd) {
	for _, agent := range m.data.status.MonitoredAgents {
		if agent.AgentID == agentID {
			return m.focusAgent(agent)
		}
	}
	m.message = "no location known for this agent"
	return m, nil
}

func (m Model) focusSelectedEscalation() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	return m.focusAgentByID(rec.AgentID)
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
	build := func(width int, _ bool) []string {
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
	// Clear the durable action banner too: renderDetail surfaces it inside the
	// overlay (for in-overlay actions like "t: see tasks" refusing), so a stale
	// list-view outcome must not leak into a freshly opened, unrelated detail.
	m.message, m.status = "", nil
	m.detail = &detailView{
		title: "Daemon captured output (last crash)",
		lines: build(m.wrapWidth(), false),
		build: build,
	}
	return m, nil
}

func (m Model) viewSelected() (tea.Model, tea.Cmd) {
	switch m.tab {
	case tabAgents:
		if agents := m.visibleAgents(); m.cursors[m.tab] < len(agents) {
			a := agents[m.cursors[m.tab]]
			build := func(width int, _ bool) []string { return m.agentDetailLines(a, width) }
			m.message, m.status = "", nil
			m.detail = &detailView{
				title: fmt.Sprintf("Agent %s", a.AgentID),
				lines: build(m.wrapWidth(), false),
				build: build,
				agent: &a,
			}
		}
	case tabTasks:
		if r := m.selectedTaskRow(); r != nil && r.item > 0 {
			row := *r
			build := func(width int, _ bool) []string { return m.taskDetailLines(row, width) }
			m.message, m.status = "", nil
			m.detail = &detailView{
				title: fmt.Sprintf("Task #%d", row.item),
				lines: build(m.wrapWidth(), false),
				build: build,
				task:  &row,
			}
		}
	case tabEscalations, tabAudit:
		if rec := m.selectedAudit(); rec != nil {
			kind := "Audit record"
			if m.tab == tabEscalations {
				kind = "Escalation"
			}
			r := *rec
			isAudit := m.tab == tabAudit
			currentPreviewLines := 10
			if isAudit {
				currentPreviewLines = 3
			}
			// Fetched once at open time (not on every resize rebuild).
			snapshot := m.app.SignatureSnapshot(m.ctx, r.Signature)
			build := func(width int, expanded bool) []string {
				return m.auditDetailLines(r, snapshot, width, auditDetailOptions{
					expanded:              expanded,
					collapseLLMOutput:     isAudit,
					currentSituationLines: currentPreviewLines,
				})
			}
			m.message, m.status = "", nil
			d := &detailView{
				title:                fmt.Sprintf("%s #%d", kind, r.ID),
				lines:                build(m.wrapWidth(), false),
				build:                build,
				hasExpandablePreview: r.PaneExcerpt != "" || snapshot != "" || (isAudit && r.LLMOutput != ""),
				// Both kinds carry a signature, so `t: see rule` works from an
				// audit detail exactly as it does from an escalation's.
				ruleDetail:    true,
				ruleSignature: r.Signature,
			}
			// Only the Escalations detail is confirmable via enter and
			// carries the per-entry actions (c/x/l), which act on this
			// snapshotted id — never the live cursor.
			if m.tab == tabEscalations {
				d.confirmID = r.ID
				d.escRetryable = m.canRetry(r)
				d.focusAgentID = r.AgentID
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
	if m.tab == tabAgents || m.tab == tabEscalations || m.tab == tabAudit || m.tab == tabSignatures {
		chrome++ // these list tabs render a column header row
	}
	if m.prompt != nil {
		if len(m.prompt.options) > 0 {
			// blank + label line + one line per choice.
			chrome += 2 + len(m.prompt.options)
		} else {
			// blank + label line + one line per line break in the expanded input.
			chrome += 2 + strings.Count(promptNewlines.Replace(m.prompt.input), "\n")
		}
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

// arriveAtTab switches to t, restoring the row and scroll position the
// operator left it on (CR-038). Both are per-tab state that simply persists;
// arriving is where they must be re-validated, because a background tab's rows
// can be deleted or filtered away under its remembered cursor. Nothing reads an
// inactive tab's cursor — every selected* helper and renderer works off m.tab —
// so clamping on arrival is both sufficient and the only correct moment.
//
// Call this LAST at a switch site: clampListViewport sizes the page via
// listPageSize, which reads m.message, m.status, m.searching, m.query[m.tab],
// m.sigMode and m.prompt — so clamping before those settle computes the offset
// against stale chrome.
func (m *Model) arriveAtTab(t tab) {
	m.tab = t
	m.clampListViewport()
}

// scrollCursorIntoView moves the active list tab's offset so the shared
// cursor stays visible (AR-003, AR-004).
func (m *Model) scrollCursorIntoView() {
	if m.tab == tabConfig {
		// Config interleaves non-selectable section headers with items, so its
		// offset is a display-LINE offset (not a row index); scroll off the
		// selected item's line position, not the raw cursor index.
		m.scrollConfigIntoView()
		return
	}
	if !m.tab.isList() {
		return
	}
	if m.cursors[m.tab] < m.offsets[m.tab] {
		m.offsets[m.tab] = m.cursors[m.tab]
	}
	if page := m.listPageSize(); m.cursors[m.tab] >= m.offsets[m.tab]+page {
		m.offsets[m.tab] = m.cursors[m.tab] - page + 1
	}
}

// scrollConfigIntoView keeps the Config tab's line offset in range and the
// selected item's display line within the visible page (the Config tab windows
// over configLines, headers included, so the title never scrolls off the top).
func (m *Model) scrollConfigIntoView() {
	lines := m.configLines()
	cursorLine := m.configCursorLine(lines)
	page := m.listPageSize()
	if cursorLine < m.offsets[tabConfig] {
		m.offsets[tabConfig] = cursorLine
	}
	if cursorLine >= m.offsets[tabConfig]+page {
		m.offsets[tabConfig] = cursorLine - page + 1
	}
	if maxOff := max(0, len(lines)-page); m.offsets[tabConfig] > maxOff {
		m.offsets[tabConfig] = maxOff
	}
	if m.offsets[tabConfig] < 0 {
		m.offsets[tabConfig] = 0
	}
}

// clampListViewport keeps every list tab's offset within
// [0, rowCount−pageSize] and the active tab's cursor within its visible
// (filtered) rows (CR-007, CR-008, CR-016). The cursor clamp stays OUTSIDE the
// list-only loop on purpose: Pause/Kill renders unwindowed but still tracks a
// cursor, and rowCountFor covers it; Config windows over display LINES, so its
// offset is reconciled by the trailing scrollCursorIntoView (→ scrollConfigIntoView).
func (m *Model) clampListViewport() {
	page := m.listPageSize()
	for t := tab(0); t < tabCount; t++ {
		if !t.isList() {
			continue
		}
		if maxOff := max(0, m.rowCountFor(t)-page); m.offsets[t] > maxOff {
			m.offsets[t] = maxOff
		}
	}
	if m.cursors[m.tab] >= m.rowCount() {
		m.cursors[m.tab] = max(0, m.rowCount()-1)
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

// detailPreviewField appends long detail content. Collapsed details show the
// requested number of trailing wrapped lines. Expanded details use the
// configured height cap (0 = unlimited). Both modes tail rather than head the
// content because actionable agent output normally lives at the bottom.
func (m Model) detailPreviewField(lines []string, width int, label, value string, expanded bool, collapsedLimit int) []string {
	if strings.TrimSpace(value) == "" {
		return lines
	}
	wrapped := wrapText(value, width)
	limit := max(1, collapsedLimit)
	mode := "preview"
	if expanded {
		limit = m.data.cfg.TUI.MaxContentHeight
		mode = "expanded"
	}
	if limit > 0 && len(wrapped) > limit {
		omitted := len(wrapped) - limit
		label = fmt.Sprintf("%s (%s: last %d of %d lines; %d earlier omitted)",
			label, mode, limit, len(wrapped), omitted)
		wrapped = wrapped[len(wrapped)-limit:]
	} else if !expanded {
		label += " (preview)"
	}
	lines = append(lines, m.styles().section.Render(label))
	for _, ln := range wrapped {
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
	status := a.Status
	if m.data.status.AgentDisabled(a.AgentID) {
		status += " [DISABLED]"
	}
	lines = m.detailField(lines, w, "Status", status)
	lines = m.detailField(lines, w, "Task source", m.agentTaskSources(a))
	if !a.At.IsZero() {
		lines = m.detailField(lines, w, "Last transition", a.At.Format(time.RFC3339))
	}
	// Lifetime stats (auto-answered, escalated, operator confirmed/corrected)
	// and the live age since first seen. Rendered as strings so zero counts
	// still show (detailField skips empty values).
	s := m.data.status.StatsFor(a.AgentID)
	lines = m.detailField(lines, w, "Escalations", strconv.Itoa(s.Escalations))
	lines = m.detailField(lines, w, "Auto-sends", strconv.Itoa(s.AutoSends))
	lines = m.detailField(lines, w, "Operator confirmed", strconv.Itoa(s.Confirmed))
	lines = m.detailField(lines, w, "Operator corrected", strconv.Itoa(s.Corrections))
	lines = m.detailField(lines, w, "Age", formatAge(s.FirstSeen, m.renderNow()))
	return lines
}

// agentTaskSourceMatches returns the cfg.TaskSources indices whose agent and
// workspace selectors match a live agent. The selector rules mirror the
// daemon's declaredTask resolver; multiple matches can come back because the
// daemon may skip a completed/unreadable source in favor of another one — or
// because a source's selectors are broad enough to also apply to other
// agents (an empty/type-level Agent selector, or a wildcard Workspace).
func (m Model) agentTaskSourceMatches(a domain.AgentTransition) []int {
	agentName := m.data.status.AgentName(a.AgentID)
	workspaceName := ""
	if ws, ok := m.data.status.Workspaces[a.WorkspaceID]; ok {
		workspaceName = ws.Label
	}
	if workspaceName == "" {
		workspaceName = a.WorkspaceID
	}

	var indices []int
	for i, src := range m.data.cfg.TaskSources {
		if src.Agent != "" && src.Agent != a.AgentID && src.Agent != a.AgentType &&
			(agentName == "" || src.Agent != agentName) {
			continue
		}
		if !domain.MatchWorkspace(src.Workspace, workspaceName) || src.Path == "" {
			continue
		}
		indices = append(indices, i)
	}
	return indices
}

// agentTaskSources returns the configured task-source paths matching a live
// agent (see agentTaskSourceMatches), joined for display; "N/A" if none.
func (m Model) agentTaskSources(a domain.AgentTransition) string {
	indices := m.agentTaskSourceMatches(a)
	if len(indices) == 0 {
		return "N/A"
	}
	paths := make([]string, len(indices))
	for i, idx := range indices {
		paths[i] = m.data.cfg.TaskSources[idx].Path
	}
	return strings.Join(paths, ", ")
}

// agentTaskCount renders the Agents list TASK column: "<total> (<pending>)"
// across every readable task source feeding the agent (an agent can match
// more than one — see agentTaskSourceMatches). "-" means no source is
// configured; "err" means every matching source failed to read, which would
// otherwise render as a truthful-looking "0 (0)". A partial read (one source
// readable, another broken) reports the plain count of what could be read,
// mirroring frontend.PendingTasks. Pending is "not done", matching the Tasks
// tab's own header — so an in-progress "[-]" item counts as neither.
func (m Model) agentTaskCount(a domain.AgentTransition) string {
	indices := m.agentTaskSourceMatches(a)
	if len(indices) == 0 {
		return "-"
	}
	total, pending, read := 0, 0, false
	for _, idx := range indices {
		// frontend.TaskGroups builds one group per cfg source, in order, so a
		// cfg index addresses its group directly (as sendTaskRow does).
		if idx >= len(m.data.tasks) {
			continue
		}
		g := m.data.tasks[idx]
		if g.Err != "" {
			continue
		}
		read = true
		total += len(g.Items)
		for _, it := range g.Items {
			if !it.Done {
				pending++
			}
		}
	}
	if !read {
		return "err"
	}
	return fmt.Sprintf("%d (%d)", total, pending)
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

// agentLocation returns the compact "#<workspace>-<tab-name>" display string
// for an agent, or "-" if workspace/tab metadata cannot be resolved. Herdr's
// tab Number is a per-workspace counter (it can collide across workspaces —
// two different workspaces can each have a tab numbered 7), while Label is
// the per-workspace position shown to the operator (commonly "1", "2", "3",
// ...), so the label is the useful locator here. Legacy/unnamed tabs fall
// back to Number.
func agentLocation(a domain.AgentTransition, status frontend.Status) string {
	if a.WorkspaceID == "" || a.TabID == "" {
		return "-"
	}
	ws, wsOk := status.Workspaces[a.WorkspaceID]
	tab, tabOk := status.Tabs[a.TabID]
	// A tab that reports a different WorkspaceID than the agent's own
	// snapshot means the two are stale relative to each other (e.g. the tab
	// moved workspaces): show "-" rather than a workspace/tab pairing that
	// doesn't actually coexist.
	if !wsOk || !tabOk || (tab.WorkspaceID != "" && tab.WorkspaceID != a.WorkspaceID) {
		return "-"
	}
	tabName := tab.Label
	if tabName == "" {
		tabName = strconv.Itoa(tab.Number)
	}
	return fmt.Sprintf("#%d-%s", ws.Number, tabName)
}

type auditDetailOptions struct {
	expanded              bool
	collapseLLMOutput     bool
	currentSituationLines int
}

func (m Model) auditDetailLines(r domain.AuditRecord, snapshot string, w int, opts auditDetailOptions) []string {
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
	lines = m.detailField(lines, w, "Confidence", frontend.ConfidenceLabel(r.Confidence))
	if r.LLMConfidence != nil {
		lines = m.detailField(lines, w, "LLM confidence",
			fmt.Sprintf("%d/100", *r.LLMConfidence))
	}
	lines = m.detailField(lines, w, "Trigger", r.Trigger)
	lines = m.detailField(lines, w, "Suggestion", r.Suggestion)
	lines = m.detailField(lines, w, "Action", r.Action)
	lines = m.detailField(lines, w, "Input", r.Input)
	lines = m.detailField(lines, w, "Rationale", r.Rationale)
	if opts.collapseLLMOutput {
		lines = m.detailPreviewField(lines, w, "LLM output", r.LLMOutput, opts.expanded, 3)
	} else {
		lines = m.detailField(lines, w, "LLM output", r.LLMOutput)
	}
	lines = m.detailField(lines, w, "Signature", r.Signature)
	if r.Signature != "" {
		if row, ok := m.ruleFor(r.Signature); ok {
			lines = m.detailField(lines, w, "Matched rule",
				frontend.RuleSummary(row, m.data.cfg.Learning.GraduationN))
			// How this situation resolved to that rule (rule-gated: no method
			// label without a rule behind it).
			if via := frontend.MatchSummary(r); via != "" {
				lines = m.detailField(lines, w, "Matched via", via)
			}
		} else {
			lines = m.detailField(lines, w, "Matched rule",
				"none yet — learned when the operator confirms or resolves this")
		}
	}
	// Embedding failure is NOT rule-gated: it is most useful exactly when a
	// paraphrase that should have matched fell back (or matched nothing)
	// because embedding was down.
	if r.EmbedError != "" {
		lines = m.detailField(lines, w, "Embedding", "failed: "+r.EmbedError)
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
		lines = m.detailPreviewField(lines, w, "Current situation", r.PaneExcerpt,
			opts.expanded, opts.currentSituationLines)
	}
	if r.Signature != "" {
		if snapshot != "" {
			lines = m.detailPreviewField(lines, w, "Original situation", snapshot, opts.expanded, 3)
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
	if m.tab == tabSignatures && m.cursors[m.tab] < len(sigs) {
		return &sigs[m.cursors[m.tab]]
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
	m.message, m.status = "", nil
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
	// TotalDecisions, not Decisions: the delete erases every row the rule holds,
	// floor or no floor.
	sig, decisions := row.Signature, row.TotalDecisions
	app, ctx := m.app, m.ctx
	m.beginAction()
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

// resetGraduationPrompt returns the selected signature to a fresh rule: shadow
// mode, zero confirmation count, and a cleared confidence (pre-reset decisions
// stop counting). Decision history is kept and the learned answer retained; the
// rule must re-earn N confirmations to re-graduate.
func (m Model) resetGraduationPrompt() (tea.Model, tea.Cmd) {
	row := m.selectedSignature()
	if row == nil {
		return m, nil
	}
	sig := row.Signature
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.prompt = &prompt{
		label: fmt.Sprintf("type 'yes' to reset %s to a fresh rule (shadow, streak → 0, confidence cleared)", shortSig(sig)),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if input != "yes" {
					return actionResultMsg{message: "reset aborted"}
				}
				reset, err := app.ResetSignatureGraduation(ctx, sig)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf(
					"reset %s to a fresh rule (shadow, streak 0, confidence cleared); history kept", shortSig(reset))}
			}
		},
	}
	return m, nil
}

// signatureDetailLines renders the full-record overlay for one signature.
func (m Model) signatureDetailLines(row frontend.SignatureRow, history []domain.DecisionRecord, graduationN, w int, expanded bool) []string {
	var lines []string
	lines = m.detailField(lines, w, "Signature", row.Signature)
	lines = m.detailField(lines, w, "Situation", string(row.SituationType))
	lines = m.detailField(lines, w, "Agent type", orDash(row.AgentType))
	lines = m.detailField(lines, w, "Mode", string(row.Mode))
	lines = m.detailField(lines, w, "Streak", fmt.Sprintf("%d/%d confirmations toward graduation", row.ConsecutiveConfirmations, graduationN))
	lines = m.detailField(lines, w, "Confidence", frontend.ConfidenceLabel(row.Confidence))
	if row.TopAction != "" {
		lines = m.detailField(lines, w, "Top action", fmt.Sprintf("%q over %d decision(s)", row.TopAction, row.Decisions))
	}
	if row.GuardState != "" {
		lines = m.detailField(lines, w, "Guard", row.GuardState)
	}
	if !row.UpdatedAt.IsZero() {
		lines = m.detailField(lines, w, "Updated", row.UpdatedAt.Format(time.RFC3339))
	}
	// Rule provenance appears with the other record fields. It is collapsed
	// by default and expands in place when the operator presses v again.
	if row.PaneExcerpt != "" {
		lines = m.detailPreviewField(lines, w, "Original situation", row.PaneExcerpt, expanded, 3)
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
	if m.tab == tabConfig && m.cursors[m.tab] < len(m.items) {
		return &m.items[m.cursors[m.tab]]
	}
	return nil
}

func (m Model) activateSelectedConfig() (tea.Model, tea.Cmd) {
	item := m.selectedRule()
	if item == nil {
		return m, nil
	}
	if item.kind != "shortcut" {
		return m.editSelectedRule()
	}
	if item.key != "install-hap" {
		return m, nil
	}

	install := m.installShortcut
	if install == nil {
		install = installHAPShortcut
	}
	m.message = ""
	m.confirm = &confirmation{
		label: "Create /usr/local/bin/hap symlink to the currently running hap binary? [Y/n]",
		onConfirm: func() tea.Cmd {
			return func() tea.Msg {
				if err := install(); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: "created /usr/local/bin/hap symlink"}
			}
		},
	}
	return m, nil
}

func (m Model) editSelectedRule() (tea.Model, tea.Cmd) {
	item := m.selectedRule()
	if item == nil {
		return m, nil
	}
	switch item.kind {
	case "scoped-pattern", "capture":
		m.message = "read-only — edit config.toml (the daemon reloads on save)"
		return m, nil
	case "shortcut":
		m.message = "press enter to run this quick shortcut"
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
	m.beginAction()
	submit := func(input string) tea.Cmd {
		return func() tea.Msg {
			if err := app.SetField(ctx, key, input); err != nil {
				return actionResultMsg{err: err}
			}
			return actionResultMsg{message: key + " updated (daemon reloaded)"}
		}
	}
	// Enum-valued fields present a picker so the operator chooses from the
	// known set instead of typing a name blind (the whole point being that
	// they may not know what values exist).
	if opts, ok := configFieldChoices(key); ok {
		cur := frontend.FieldValue(m.data.cfg, key)
		idx := 0
		for i, o := range opts {
			if o == cur {
				idx = i
				break
			}
		}
		m.prompt = &prompt{
			label:    fmt.Sprintf("select %s (↑/↓ then enter, current %s)", key, cur),
			options:  opts,
			optIdx:   idx,
			onSubmit: submit,
		}
		return m, nil
	}
	m.prompt = &prompt{
		label:    fmt.Sprintf("set %s (current %s)", key, frontend.FieldValue(m.data.cfg, key)),
		onSubmit: submit,
	}
	return m, nil
}

// configFieldChoices returns the fixed value set for an enum-valued config
// field (rendered as a picker in the TUI), or ok=false for free-value fields.
func configFieldChoices(key string) (choices []string, ok bool) {
	switch key {
	case "tui.theme":
		return config.ValidThemes, true
	default:
		return nil, false
	}
}

func (m Model) addPatternPrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.beginAction()
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
	m.beginAction()
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

// showSelectedAgentTasks jumps to the Tasks tab for the agent under the
// cursor (t on the Agents list), mirroring focusSelected's "f".
func (m Model) showSelectedAgentTasks() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	return m.showAgentTasks(agents[m.cursors[m.tab]])
}

// showAgentTasks jumps to the Tasks tab with the given agent's task source
// selected (its header row under the cursor), so the agent's checklist is one
// keystroke away instead of a hunt through every configured source — shared by
// the Agents list and detail "t". A source's Agent/Workspace selectors can be
// broad enough to match several entries (see agentTaskSourceMatches); unlike a
// destructive clear, selecting one is safe to guess, so the first match wins
// and the banner names the rest. Removing a task source stays on the Config
// tab ("x: remove").
func (m Model) showAgentTasks(a domain.AgentTransition) (tea.Model, tea.Cmd) {
	indices := m.agentTaskSourceMatches(a)
	if len(indices) == 0 {
		m.message = "no task source configured for this agent — add one on the Config tab (t)"
		m.scrollCursorIntoView() // the hint line shrinks the page
		return m, nil
	}
	group := indices[0]

	m.detail = nil
	m.tab = tabTasks
	m.searching = false
	m.message = ""
	cursor, ok := m.taskGroupHeaderRow(group)
	if !ok && m.query[tabTasks] != "" {
		// The Tasks search filter hides the very row being jumped to; a jump
		// that lands nowhere is worse than a dropped filter, so clear it.
		m.query[tabTasks] = ""
		cursor, ok = m.taskGroupHeaderRow(group)
	}
	m.offsets[tabTasks] = 0
	if !ok {
		// cfg lists the source but the daemon hasn't reported its task list
		// yet (the poll and the config read can disagree for a tick).
		m.cursors[m.tab] = 0
		m.message = fmt.Sprintf("task source #%d (%s) isn't loaded yet — it appears on the next refresh",
			group, m.data.cfg.TaskSources[group].Path)
		return m, nil
	}
	m.cursors[m.tab] = cursor
	if len(indices) > 1 {
		paths := make([]string, 0, len(indices)-1)
		for _, idx := range indices[1:] {
			paths = append(paths, m.data.cfg.TaskSources[idx].Path)
		}
		m.message = fmt.Sprintf("agent matches %d task sources — showing the first; also: %s",
			len(indices), strings.Join(paths, ", "))
	}
	m.scrollCursorIntoView() // after the banner: it shrinks the page by 2
	return m, nil
}

// taskGroupHeaderRow locates a task source's header row among the currently
// visible (filtered) Tasks rows, reporting whether the search filter or a
// not-yet-loaded task list left it off screen.
func (m Model) taskGroupHeaderRow(group int) (int, bool) {
	for i, r := range m.visibleTaskRows() {
		if r.header && r.group == group {
			return i, true
		}
	}
	return 0, false
}

// showSelectedRule jumps to the Rules tab for the escalation/audit row under
// the cursor (t on either list), mirroring showSelectedAgentTasks. selectedAudit
// already serves both tabs and bounds-checks the cursor.
func (m Model) showSelectedRule() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	return m.showRuleFor(rec.Signature)
}

// showRuleFor jumps to the Rules tab with the rule a record is keyed to already
// selected (AR-039) — shared by the Escalations/Audit lists and their detail
// overlays, since a record and its rule share the signature string (see
// ruleFor). Reading the rule behind a decision otherwise means switching tabs
// and matching the id by eye.
//
// The two "can't jump" cases are different truths and get different messages: no
// signature at all means the mask guard tripped and this situation can NEVER
// have a rule, while a signature with no rule yet just means nobody has
// confirmed one.
func (m Model) showRuleFor(signature string) (tea.Model, tea.Cmd) {
	if signature == "" {
		m.message = "no signature on this record — an over-masked situation never matches a rule"
		m.scrollCursorIntoView() // the hint line shrinks the page
		return m, nil
	}
	if _, ok := m.ruleFor(signature); !ok {
		m.message = fmt.Sprintf("no rule learned for %s yet — one appears once you confirm or resolve it",
			shortSig(signature))
		m.scrollCursorIntoView()
		return m, nil
	}

	m.detail = nil
	m.searching = false
	m.message = ""
	cursor, ok := m.ruleRowFor(signature)
	if !ok {
		// Hidden by the Rules tab's own filters — it composes a search query
		// AND the f mode cycle, so either can bury the target. ruleFor already
		// proved the rule exists, so clearing both makes the retry land.
		m.query[tabSignatures] = ""
		m.sigMode = ""
		cursor, ok = m.ruleRowFor(signature)
	}
	m.tab = tabSignatures
	m.offsets[tabSignatures] = 0
	if !ok {
		// Unreachable today — with both filters cleared visibleSignatures is
		// m.data.signatures verbatim, which ruleFor just found the rule in.
		// Kept as a guard in case the Rules tab grows a third filter that this
		// jump doesn't know to clear.
		m.cursors[m.tab] = 0
		m.message = fmt.Sprintf("rule %s is no longer listed — refresh and retry", shortSig(signature))
		m.scrollCursorIntoView()
		return m, nil
	}
	m.cursors[m.tab] = cursor
	m.scrollCursorIntoView()
	return m, nil
}

// ruleRowFor locates a rule among the currently visible (filtered) Rules rows,
// reporting whether the search query or the sigMode filter left it off screen.
// ruleFor is the unfiltered lookup and returns the row, not a cursor position.
func (m Model) ruleRowFor(signature string) (int, bool) {
	for i, r := range m.visibleSignatures() {
		if r.Signature == signature {
			return i, true
		}
	}
	return 0, false
}

func (m Model) removeSelectedRule() (tea.Model, tea.Cmd) {
	item := m.selectedRule()
	if item == nil {
		return m, nil
	}
	app := m.app
	switch item.kind {
	case "pattern":
		m.beginAction()
		idx, expected := item.index, item.value
		return m, m.do(fmt.Sprintf("never-auto pattern #%d removed", idx), func(c context.Context) error {
			return app.RemoveNeverAutoPattern(c, idx, expected)
		})
	case "source":
		m.beginAction()
		idx, expected := item.index, item.value
		return m, m.do(fmt.Sprintf("task source #%d removed", idx), func(c context.Context) error {
			return app.RemoveTaskSource(c, idx, expected)
		})
	case "scoped-pattern", "capture":
		m.message = "read-only — edit config.toml (the daemon reloads on save)"
		return m, nil
	case "shortcut":
		m.message = "quick shortcuts can't be removed"
		return m, nil
	default:
		m.message = "config fields are edited (enter), not removed"
		return m, nil
	}
}

func (m Model) clearDataPrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.beginAction()
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
		if i == int(tabTasks) {
			if p := frontend.PendingTasks(m.data.tasks); p > 0 {
				label = fmt.Sprintf(" %s(%d) ", name, p)
			}
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
	case tabTasks:
		m.renderTasks(&b)
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

	if m.prompt != nil && len(m.prompt.options) > 0 {
		// Picker mode: label then one row per choice, the highlight marked.
		fmt.Fprintf(&b, "\n%s\n", m.prompt.label)
		for i, opt := range m.prompt.options {
			marker := "  "
			if i == m.prompt.optIdx {
				marker = "❯ "
			}
			fmt.Fprintf(&b, "%s%s\n", marker, opt)
		}
	} else if m.prompt != nil {
		// A multiline input expands the box: one rendered line per line break,
		// continuation lines indented under the label, cursor on the last.
		lines := strings.Split(promptNewlines.Replace(m.prompt.input), "\n")
		fmt.Fprintf(&b, "\n%s> %s", m.prompt.label, lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(&b, "\n  %s", l)
		}
		fmt.Fprint(&b, "█\n")
	}
	if m.confirm != nil {
		fmt.Fprintf(&b, "\n%s\n", m.confirm.label)
	}
	if m.message != "" {
		fmt.Fprintf(&b, "\n%s\n", m.message)
	}
	// Durable status area: the last action outcome stays readable until the
	// next mutation starts, styled ok/error from the palette (CR-026).
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
		preview := ""
		closeKeys := "esc/q/v: close"
		if m.detail.hasExpandablePreview {
			closeKeys = "esc/q: close"
			if m.detail.previewExpanded {
				preview = "  v: collapse previews"
			} else {
				preview = "  v: expand previews"
			}
		}
		// Derived from the marker that actually gates `t`, not from confirmID:
		// an audit detail is rule-bearing too, but carries no confirmID (only a
		// pending escalation does), so keying the hint off confirmID would leave
		// the key working and unadvertised on the Audit tab — where "which rule
		// decided this?" is the likeliest question.
		rule := ""
		if m.detail.ruleDetail {
			rule = "  t: see rule"
		}
		if m.detail.confirmID != 0 {
			retry := ""
			if m.detail.escRetryable {
				retry = "  l: retry LLM"
			}
			return "enter: confirm+send  c: correct (+send?)  x: delete  f: focus in herdr" + rule + retry +
				preview + "  ↑/↓: scroll  tab: switch tab  " + closeKeys
		}
		if m.detail.agent != nil {
			return "x: disable  e: enable  ↑/↓: scroll  tab: switch tab  f: focus in herdr  t: see tasks" + preview + "  " + closeKeys
		}
		if m.detail.task != nil {
			return "enter/y: send to agent  e: edit  x: delete  f: focus in herdr  ↑/↓: scroll  tab: switch tab  " + closeKeys
		}
		return "↑/↓: scroll  tab: switch tab" + rule + preview + "  " + closeKeys
	}
	if m.searching {
		return "type to filter  backspace: erase  esc/enter: apply & close"
	}
	if m.confirm != nil {
		return "y/enter: confirm  n/esc: cancel"
	}
	common := "tab: switch  ↑/↓: select  p: pause  r: resume  q: quit"
	if d := m.data.status.Drift; d.Detected && !d.ModelMissing {
		common = "R: re-embed  " + common
	}
	switch m.tab {
	case tabAgents:
		return "v: details  x: disable  e: enable  n: rename agent  f: focus in herdr  t: see tasks  /: search  " + common
	case tabTasks:
		return "enter/y: send to agent  v: details  a: add  e: edit  d: done/undone  x: delete (source on a header)  space: mark  f: focus in herdr  /: search  " + common
	case tabEscalations:
		return "enter/y: confirm+send  c: correct (+send?)  l: retry LLM  f: focus in herdr  t: see rule  space: mark  x: delete  X: prune old  v: details  /: search  " + common
	case tabAudit:
		return "c: correct decision  v: details  t: see rule  /: search  " + common
	case tabSignatures:
		return "enter/v: details  x: delete  0: reset  f: filter mode  /: search  " + common
	case tabConfig:
		return "enter: edit/run shortcut  e: edit field  a: add pattern  t: add task source  x: remove  X: clear data  " + common
	}
	return common
}

// renderTasks draws the aggregated task list of every configured task source
// (the Tasks tab): a header row per source, its checklist items under it.
func (m Model) renderTasks(b *strings.Builder) {
	st := m.styles()
	rows := m.visibleTaskRows()
	if len(rows) == 0 {
		if len(m.data.tasks) > 0 {
			fmt.Fprintln(b, st.help.Render("no tasks match the filter — / edits, backspace clears"))
			return
		}
		fmt.Fprintln(b, st.help.Render("no task sources configured — press t on the Config tab, or: hap task-source add"))
		return
	}
	start, end := m.window(len(rows))
	for i := start; i < end; i++ {
		r := rows[i]
		line := r.text
		switch {
		case i == m.cursors[m.tab]:
			line = st.selected.Render(line)
		case r.header:
			line = st.section.Render(line)
		case r.errRow:
			line = st.err.Render(line)
		case r.inProgress:
			line = st.warn.Render(line)
		case r.done:
			line = st.help.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(rows)-end)
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
	// renders untruncated (CR-032); the fixed columns after it are 52
	// cells, so the action budget shifts right with the column.
	sigW := 18
	for _, r := range sigs {
		if n := runewidth.StringWidth(r.Signature); n > sigW {
			sigW = n
		}
	}
	// LAST replaces the old SITUATION column: the situation type is already the
	// signature id's prefix (e.g. "approval:9f2c"), so the column instead shows
	// when the rule was last used — its most recent audit entry, an auto-act OR
	// an escalation — humanized and ticking like the WHEN columns. It is 12 wide
	// to fit "5h 59m ago" and the ≥ 6h timestamp fallback; "-" until first use.
	const rulesRowFmt = "%-*s %-12s %-10s %5s %-11s %7s  %s"
	actWidth, _ := m.budget(sigW+52, false)
	header := fmt.Sprintf(rulesRowFmt, sigW,
		"SIGNATURE", "LAST", "TYPE", "CONF", "MODE", "CONFIRM", "TOP ACTION")
	fmt.Fprintln(b, st.section.Render(header))
	start, end := m.window(len(sigs))
	for i := start; i < end; i++ {
		r := sigs[i]
		var lastUsed time.Time
		if r.LastAudit != nil {
			lastUsed = r.LastAudit.CreatedAt
		}
		line := fmt.Sprintf(rulesRowFmt,
			sigW, r.Signature, humanizeWhen(lastUsed, m.renderNow()), orDash(r.AgentType),
			frontend.ConfidenceLabel(r.Confidence), r.Mode,
			fmt.Sprintf("%d/%d", r.ConsecutiveConfirmations, gradN),
			oneLine(r.TopAction, actWidth))
		switch {
		case i == m.cursors[m.tab]:
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
	switch {
	case start > 0 && end < len(lines):
		fmt.Fprintf(b, "%s\n", st.help.Render(fmt.Sprintf(
			"… %d earlier / %d later line(s) — ↑/↓ to scroll", start, len(lines)-end)))
	case start > 0:
		fmt.Fprintf(b, "%s\n", st.help.Render(fmt.Sprintf("… %d earlier line(s) — ↑ to scroll", start)))
	case end < len(lines):
		fmt.Fprintf(b, "%s\n", st.help.Render(fmt.Sprintf("… %d more line(s) — ↓ to scroll", len(lines)-end)))
	}
	// Per-entry actions available from inside the overlay (e.g. "t: see
	// tasks") report their outcome the same way list-view actions do —
	// without these, a refusal (no match) or a success banner would be
	// silently invisible while the overlay stays open.
	if m.message != "" {
		fmt.Fprintf(b, "\n%s\n", m.message)
	}
	if m.status != nil {
		mark, style := "✓", st.ok
		if m.status.err {
			mark, style = "✗", st.err
		}
		text := oneLine(m.status.text, max(20, m.contentWidth()-12))
		fmt.Fprintf(b, "\n%s\n", style.Render(
			fmt.Sprintf("%s %s  %s", mark, text, m.status.at.Format("15:04:05"))))
	}
	fmt.Fprintf(b, "\n%s", st.help.Render(m.helpLine()))
}

// agentsRowFmt lays out the Agents list: name, id, type, status (all fixed
// width so the trailing numeric columns line up), the agent's task count, then
// the four lifetime counters right-aligned and the live age last.
const agentsRowFmt = "%-18s %-12s %-12s %-10s %7s %5s %5s %5s %5s  %s"

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
	// Rows are clamped to the content width: a wrapped line would break the
	// one-row-one-line accounting window()/listPageSize() depend on, exactly
	// as renderTasks guards its own headers.
	rowWidth := max(20, m.contentWidth())
	header := fmt.Sprintf(agentsRowFmt,
		"NAME", "LOCATION", "TYPE", "STATUS", "TASK", "ESCA", "AUTO", "CONF", "CORR", "AGE")
	fmt.Fprintln(b, m.styles().section.Render(oneLine(header, rowWidth)))
	now := m.renderNow()
	start, end := m.window(len(agents))
	for i := start; i < end; i++ {
		a := agents[i]
		name := orDash(m.data.status.AgentName(a.AgentID))
		s := m.data.status.StatsFor(a.AgentID)
		status := a.Status
		if m.data.status.AgentDisabled(a.AgentID) {
			status = "DISABLED"
		}
		line := fmt.Sprintf(agentsRowFmt,
			name, oneLine(agentLocation(a, m.data.status), 12), a.AgentType, status,
			oneLine(m.agentTaskCount(a), 7),
			strconv.Itoa(s.Escalations), strconv.Itoa(s.AutoSends),
			strconv.Itoa(s.Confirmed), strconv.Itoa(s.Corrections),
			formatAge(s.FirstSeen, now))
		line = oneLine(line, rowWidth)
		if i == m.cursors[m.tab] {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(agents)-end)
}

// formatAge renders the elapsed time since firstSeen as HH:MM:SS (hours may
// exceed 24). It returns "-" when firstSeen is zero (first-seen unknown).
func formatAge(firstSeen, now time.Time) string {
	if firstSeen.IsZero() {
		return "-"
	}
	d := now.Sub(firstSeen)
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// humanizeWhen renders how long ago an escalation was raised in a compact,
// human-friendly form that advances in real time against the caller's clock
// (like the Agents tab Age, driven by the 1s clockTick). Under six hours it
// counts up in seconds, then minutes, then hours+minutes ("30s ago", "5m ago",
// "1h 45m ago", "4h 00m ago", "5h 59m ago"); at or beyond six hours a precise
// point in time is more useful than an ever-growing relative count, so it shows
// the exact wall-clock timestamp instead ("Jul 19 14:30"). Returns "-" when the
// timestamp is zero.
func humanizeWhen(created, now time.Time) string {
	if created.IsZero() {
		return "-"
	}
	d := now.Sub(created)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 6*time.Hour:
		return fmt.Sprintf("%dh %02dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return created.Format("Jan 2 15:04")
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
	esc := m.visibleEscalations()
	if len(esc) == 0 {
		if len(m.data.escalations) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no escalations match the filter — / edits, backspace clears"))
		} else {
			fmt.Fprintln(b, m.styles().help.Render("no pending escalations — the herd is unblocked 🎉"))
		}
		return
	}
	// The final details column shares the remaining width between rationale
	// and suggestion. LLM is the consulting model's self-reported 0-100 ("-"
	// when the escalation carries no score, e.g. shadow mode or a safety veto).
	const (
		// SITUATION must fit "unclassifiable", the longest domain value.
		// Allowing it to overflow shifts TYPE and every column after it away
		// from their headers.
		// WHEN is 12 wide to fit the humanized age ("5h 59m ago") and the
		// exact-timestamp fallback ("Jul 19 14:30") used at ≥ 6h.
		escRowFmt = "%-1s %-6s %-12s %-14s %-8s %-14s %4s %-6s %5s  %s"
		escPrefix = 80
	)
	header := fmt.Sprintf(escRowFmt,
		"", "ID", "WHEN", "SITUATION", "TYPE", "AGENT", "LLM", "RULE", "CONF", "RATIONALE / SUGGESTION")
	fmt.Fprintln(b, m.styles().section.Render(header))
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
		line := fmt.Sprintf(escRowFmt,
			mark, fmt.Sprintf("#%d", e.ID), humanizeWhen(e.CreatedAt, m.renderNow()), e.SituationType,
			oneLine(orDash(m.agentTypeFor(e)), 8), oneLine(agent, 14),
			llmConfShort(e.LLMConfidence), m.ruleMarker(e.Signature), frontend.ConfidenceLabel(e.Confidence),
			oneLine(e.Rationale, rWidth))
		if e.Suggestion != "" {
			line += "  → " + oneLine(e.Suggestion, sWidth)
		}
		if i == m.cursors[m.tab] {
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
	// The final Action column takes the remaining width after these fixed
	// columns. Conf is the computed 0-1 agreement ("-" when the row was never
	// scored — see frontend.ConfidenceLabel); LLM is the consulting model's
	// self-reported 0-100 ("-" when the row has no LLM score).
	const auditRowFmt = "%-6s %-14s %-10s %-8s %-14s %4s %-6s %5s %-9s  %s"
	actWidth, _ := m.budget(86, false)
	header := fmt.Sprintf(auditRowFmt,
		"ID", "WHEN", "SITUATION", "TYPE", "AGENT", "LLM", "RULE", "CONF", "STATUS", "ACTION")
	fmt.Fprintln(b, m.styles().section.Render(header))
	start, end := m.window(len(rows))
	for i := start; i < end; i++ {
		r := rows[i]
		agent := m.data.status.AgentName(r.AgentID)
		if agent == "" {
			agent = r.AgentID
		}
		line := fmt.Sprintf(auditRowFmt,
			fmt.Sprintf("#%d", r.ID), humanizeWhen(r.CreatedAt, m.renderNow()),
			r.SituationType, oneLine(orDash(m.agentTypeFor(r)), 8), oneLine(orDash(agent), 14),
			llmConfShort(r.LLMConfidence), m.ruleMarker(r.Signature), frontend.ConfidenceLabel(r.Confidence), r.Status,
			oneLine(r.Action, actWidth))
		if i == m.cursors[m.tab] {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(rows)-end)
}

// configLine is one display row of the Config tab: either a section header or
// blank spacer (itemIdx = -1, not selectable) or a selectable item row
// (itemIdx into m.items). Flattening headers and items into one ordered line
// list lets the tab window/scroll like the other list tabs so the title row is
// never pushed off the top when the content outgrows the pane.
type configLine struct {
	text    string
	itemIdx int
}

// configLines flattens the Config tab into its ordered display lines. Item
// rows carry their m.items index and are rendered unstyled here (the selected
// highlight is applied at draw time); headers and the empty-section notices
// are pre-styled and non-selectable.
func (m Model) configLines() []configLine {
	st := m.styles()
	var lines []configLine
	header := func(s string, blankBefore bool) {
		if blankBefore {
			lines = append(lines, configLine{text: "", itemIdx: -1})
		}
		lines = append(lines, configLine{text: st.section.Render(s), itemIdx: -1})
	}
	emptySections := func() {
		if len(m.data.cfg.Safety.NeverAutoPatterns) == 0 && len(m.data.cfg.Safety.NeverAutoRules) == 0 {
			header(fmt.Sprintf("Never-auto patterns: none from operator (%s) — press a to add", m.seedLabel()), true)
		}
		if len(m.data.cfg.TaskSources) == 0 {
			header("Task sources: none — press t to add", false)
		}
	}
	lastKind := ""
	emptySectionsRendered := false
	for i, item := range m.items {
		if item.kind != lastKind {
			// Empty mutable sections still belong above Quick Shortcuts, which
			// is intentionally the final section in the Config tab.
			if item.kind == "shortcut" && !emptySectionsRendered {
				emptySections()
				emptySectionsRendered = true
			}
			switch item.kind {
			case "field":
				header("Config", false)
			case "pattern":
				header(fmt.Sprintf("Never-auto patterns (operator; %s)", m.seedLabel()), true)
			case "source":
				header("Task sources", true)
			case "scoped-pattern":
				header("Scoped never-auto rules (read-only — edit config.toml)", true)
			case "capture":
				header("Capture delays (read-only — edit config.toml)", true)
			case "shortcut":
				header("Quick Shortcuts", true)
			}
			lastKind = item.kind
		}
		// Long values (argv templates, paths) truncate to one line (CR-037).
		lines = append(lines, configLine{text: "  " + oneLine(item.label, m.contentWidth()-2), itemIdx: i})
	}
	if !emptySectionsRendered {
		emptySections()
	}
	return lines
}

// configCursorLine maps the selected item index to its position in the flat
// configLines list (0 when the cursor's item isn't found — e.g. an empty tab).
func (m Model) configCursorLine(lines []configLine) int {
	for i, ln := range lines {
		if ln.itemIdx == m.cursors[tabConfig] {
			return i
		}
	}
	return 0
}

func (m Model) renderConfig(b *strings.Builder) {
	st := m.styles()
	if len(m.items) == 0 {
		fmt.Fprintln(b, st.help.Render("no configuration loaded"))
		return
	}
	lines := m.configLines()
	start, end := m.window(len(lines))
	for i := start; i < end; i++ {
		text := lines[i].text
		if lines[i].itemIdx >= 0 && lines[i].itemIdx == m.cursors[tabConfig] {
			text = st.selected.Render(text)
		}
		fmt.Fprintln(b, text)
	}
	m.renderMoreRows(b, len(lines)-end)
}

// seedLabel names the shipped seed patterns' state: the count when active, or
// an explicit marker when safety.disable_never_auto_seed_patterns dropped them,
// so the Config tab never contradicts the editable field.
func (m Model) seedLabel() string {
	if m.data.cfg.Safety.DisableNeverAutoSeedPatterns {
		return "seed disabled"
	}
	return fmt.Sprintf("+%d seed active", domain.SeedNeverAutoRuleCount())
}

func (m Model) renderKills(b *strings.Builder) {
	if len(m.data.kills) == 0 {
		fmt.Fprintln(b, m.styles().help.Render("no pause/kill events recorded"))
		return
	}
	for i, e := range m.data.kills {
		line := fmt.Sprintf("#%-4d %-20s %-8s by %s",
			e.ID, e.CreatedAt.Format(time.RFC3339), e.State, e.Author)
		if i == m.cursors[m.tab] {
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

// llmConfShort renders an audit row's LLM confidence for a list column: the
// 0-100 score, or "-" when the row has no LLM score (learned/operator rows).
func llmConfShort(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

// Run starts the TUI program.
func Run(ctx context.Context, app *frontend.App) error {
	m := New(ctx, app)
	m.bellOut = os.Stdout
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	// bubbletea does not wait for in-flight Cmd goroutines on quit; drain
	// them (bounded, in case a Cmd was somehow never launched) so a send
	// confirmed right before quitting still lands and registers its submit
	// retries before main's exit drain runs.
	done := make(chan struct{})
	go func() { m.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
	return err
}
