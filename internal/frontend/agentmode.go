package frontend

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// Reading and setting an agent's permission mode (Claude's Shift+Tab cycle,
// Codex's Default/Plan toggle).
//
// Neither herdr nor either agent exposes the mode over any API, so the read is a
// pane parse (internal/domain/agentmode.go) and the write is a keystroke. That
// makes this an OPEN LOOP with a verified close: every press is followed by a
// re-read, and the loop stops on what the pane reports, never on what the send
// returned. herdr reports success for a chord it silently mangles (see
// herdr.CLI.SendChord), so a green send is not evidence of anything.

// ErrModeUnsupported means the agent type has no Shift+Tab mode toggle hap
// knows about. Callers surface it as a plain refusal, not a failure.
var ErrModeUnsupported = errors.New("agent type has no permission-mode toggle")

// ErrModeUnreadable means the pane did not show a mode indicator. It is
// deliberately distinct from "the mode is X": absence of an indicator means the
// footer is not visible (a modal is up, the pane is mid-repaint, the capture
// failed), and treating it as a default would let `set manual` no-op over an
// agent that is actually in auto mode.
var ErrModeUnreadable = errors.New("could not read the agent's mode from its pane")

// ErrModeUnsafe means the pane is not showing its ordinary composer, so
// Shift+Tab does not mean "cycle the mode" right now. Claude rebinds it inside
// modals — a standing plan approval renders "shift+tab to approve with this
// feedback" — so pressing anyway would answer the modal.
var ErrModeUnsafe = errors.New("agent is not at its composer; refusing to send keystrokes")

// ModeReport is what one agent's mode read resolved to.
type ModeReport struct {
	AgentID   string
	AgentName string
	AgentType string
	PaneID    string
	Mode      domain.AgentMode
}

// Label names the agent the way the operator asked for it, falling back to
// whatever identity resolved. Shared by every message about a mode so the CLI
// does not restate the same fallback chain.
func (r ModeReport) Label(target string) string {
	switch {
	case target != "":
		return target
	case r.AgentName != "":
		return r.AgentName
	default:
		return r.AgentID
	}
}

// ModeChange records what SetAgentMode did.
type ModeChange struct {
	ModeReport
	// From is the mode observed before any keystroke; Mode is the mode the
	// pane reported at the end.
	From domain.AgentMode
	// Presses counts the Shift+Tab chords actually delivered. Zero means the
	// agent was already in the target mode and nothing was sent.
	Presses int
}

// ModeOptions tunes the rotate-until-target loop. The zero value is the
// production setting; tests shrink the waits.
type ModeOptions struct {
	// SettleTimeout bounds how long one press is given to show up in the pane.
	SettleTimeout time.Duration
	// PollInterval is how often the pane is re-read while waiting.
	PollInterval time.Duration
}

// Production defaults. The loop WAITS FOR THE MODE TO CHANGE rather than
// sleeping a fixed delay and pressing again, which is what keeps it correct at
// both ends: a slow repaint does not cause a second press (which would overshoot
// the target by one and, on Codex's two-mode toggle, land exactly back where it
// started), and a fast one does not cost a fixed delay per step.
const (
	defaultModeSettleTimeout = 3 * time.Second
	defaultModePollInterval  = 200 * time.Millisecond
)

func (o ModeOptions) settleTimeout() time.Duration {
	if o.SettleTimeout > 0 {
		return o.SettleTimeout
	}
	return defaultModeSettleTimeout
}

func (o ModeOptions) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return defaultModePollInterval
}

// modeReadLines is how much of the pane to read. Generous: Claude renders its
// subagent task tray below the mode line, so the indicator is not always at the
// bottom, and the domain parser bounds its own footer window anyway.
const modeReadLines = 60

// FindLiveAgent resolves an operator-typed target to a live agent. Exact
// agent/pane ids take precedence over the operator-assigned short name, so an
// id always means itself even if someone named another agent after it.
func (a *App) FindLiveAgent(ctx context.Context, target string) (domain.AgentTransition, error) {
	if a.Herdr == nil {
		return domain.AgentTransition{}, fmt.Errorf("herdr is unavailable")
	}
	agents, err := a.Herdr.ListAgents(ctx)
	if err != nil {
		return domain.AgentTransition{}, fmt.Errorf("listing live agents: %w", err)
	}
	for i := range agents {
		if agents[i].AgentID == target || agents[i].PaneID == target {
			return agents[i], nil
		}
	}
	names, err := a.Store.AgentNames(ctx)
	if err != nil {
		return domain.AgentTransition{}, err
	}
	for i := range agents {
		if names[agents[i].AgentID] == target {
			return agents[i], nil
		}
	}
	return domain.AgentTransition{}, fmt.Errorf("live agent %q not found", target)
}

// AgentMode reports the permission mode a live agent is currently in. It sends
// nothing and is safe to call against any pane, including one sitting at a
// modal — such a pane simply reports ErrModeUnreadable.
func (a *App) AgentMode(ctx context.Context, target string) (ModeReport, error) {
	agent, err := a.FindLiveAgent(ctx, target)
	if err != nil {
		return ModeReport{}, err
	}
	report, readErr := a.modeReportFor(ctx, agent)
	if domain.AgentModesFor(agent.AgentType) == nil {
		return report, fmt.Errorf("%w: %q", ErrModeUnsupported, agent.AgentType)
	}
	// A FAILED read and an unreadable SCREEN are different problems with
	// different fixes, and collapsing them was actively misleading: a recycled
	// pane id (`pane_not_found`) surfaced as "an approval or form is probably up
	// — answer it, then retry", sending the operator to look for a modal that
	// does not exist while the real cause never reached them.
	if readErr != nil {
		return report, readErr
	}
	if report.Mode == domain.AgentModeUnknown {
		return report, ErrModeUnreadable
	}
	return report, nil
}

// modeReportFor builds the report skeleton and fills in whatever the pane
// currently shows, returning any READ failure separately from an unreadable
// screen. Callers that only want a display value (FillAgentModes) ignore the
// error; callers reporting to an operator must not.
func (a *App) modeReportFor(ctx context.Context, agent domain.AgentTransition) (ModeReport, error) {
	report := ModeReport{
		AgentID:   agent.AgentID,
		AgentType: agent.AgentType,
		PaneID:    panePreferred(agent),
	}
	if names, err := a.Store.AgentNames(ctx); err == nil {
		report.AgentName = names[agent.AgentID]
	}
	pane, err := a.readModePane(ctx, report.PaneID)
	if err != nil {
		return report, fmt.Errorf("reading pane %s: %w", report.PaneID, err)
	}
	report.Mode, _ = domain.AgentModeFromPane(agent.AgentType, pane)
	return report, nil
}

// panePreferred picks the pane id to act on. herdr identifies most agents by
// their pane id and the transition carries both, so falling back keeps an empty
// PaneID from dropping the agent.
func panePreferred(agent domain.AgentTransition) string {
	if agent.PaneID != "" {
		return agent.PaneID
	}
	return agent.AgentID
}

// readModePane reads the pane's CURRENT screen.
//
// It must be `--source visible`, never ReadPane's `--source recent`: recent is a
// CONSUMING delta shared with the daemon's classification read, so polling it
// here in a loop would eat the content the daemon needs to classify the very
// screen we are pressing keys into. Visible is also the only source that
// reflects the repaint after a keystroke, which is the whole point of the wait.
func (a *App) readModePane(ctx context.Context, paneID string) (string, error) {
	reader, ok := a.Herdr.(ports.VisiblePaneReader)
	if !ok {
		return "", fmt.Errorf("this herdr adapter cannot read a pane's visible content")
	}
	return reader.ReadPaneVisible(ctx, paneID, modeReadLines)
}

// SetAgentMode rotates a live agent to the requested permission mode by pressing
// Shift+Tab until the pane reports it, and returns what actually happened.
//
// The safety shape, in order, and every step matters:
//
//  1. The target must be a mode this agent type can actually reach. Codex can
//     never report "auto", so accepting it would burn the whole press ceiling
//     rotating a two-mode toggle.
//  2. The mode is read BEFORE anything is sent. An unreadable pane refuses
//     outright — pressing blind is how a "set the mode" turns into an answered
//     modal.
//  3. Already in the target mode is a NO-OP that sends nothing. This is what
//     makes the command safe to call from a script on every iteration.
//  4. Composer readiness is re-checked before EVERY press, not once up front.
//     The agent is live: a permission prompt can appear between two presses, and
//     the press after it would answer the prompt.
//  5. The loop stops on what the pane REPORTS. herdr returns success for a chord
//     it delivers as a bare TAB, so the send's exit code proves nothing.
func (a *App) SetAgentMode(ctx context.Context, target, modeName string, opts ModeOptions) (ModeChange, error) {
	agent, err := a.FindLiveAgent(ctx, target)
	if err != nil {
		return ModeChange{}, err
	}
	want, ok := domain.ParseAgentMode(agent.AgentType, modeName)
	if !ok {
		if modes := domain.AgentModesFor(agent.AgentType); modes != nil {
			return ModeChange{}, fmt.Errorf("%q is not a mode for a %s agent (want one of %s)",
				modeName, agent.AgentType, joinModes(modes))
		}
		return ModeChange{}, fmt.Errorf("%w: %q", ErrModeUnsupported, agent.AgentType)
	}

	chord, ok := a.Herdr.(ports.ChordSender)
	if !ok {
		return ModeChange{}, fmt.Errorf("this herdr adapter cannot send the shift+tab chord")
	}

	change := ModeChange{ModeReport: ModeReport{
		AgentID:   agent.AgentID,
		AgentType: agent.AgentType,
		PaneID:    panePreferred(agent),
	}}
	if names, err := a.Store.AgentNames(ctx); err == nil {
		change.AgentName = names[agent.AgentID]
	}

	pane, err := a.readModePane(ctx, change.PaneID)
	if err != nil {
		return change, fmt.Errorf("reading pane %s: %w", change.PaneID, err)
	}
	current, ok := domain.AgentModeFromPane(agent.AgentType, pane)
	if !ok {
		return change, ErrModeUnreadable
	}
	change.From = current
	change.Mode = current

	// bypassPermissions is entered with --dangerously-skip-permissions at
	// launch and is not part of the Shift+Tab rotation, so an agent sitting in
	// it can never leave. Caught here rather than by the press ceiling: the
	// ceiling would spend its whole budget delivering chords that do nothing,
	// then report a generic "still in bypassPermissions" that does not tell the
	// operator the mode is structurally unreachable.
	if current == domain.AgentModeBypass {
		return change, fmt.Errorf("%s was launched with --dangerously-skip-permissions and is in "+
			"%s mode, which the shift+tab cycle cannot leave — restart the agent without that flag",
			change.Label(target), current)
	}

	// seen records the modes this session has actually offered. Returning to one
	// means the rotation has closed WITHOUT passing through the target, which is
	// how an unavailable mode is detected — and it is not hypothetical: a claude
	// session's cycle is per-SESSION, not per-agent-type. Verified live
	// (2026-08-09) a `--model haiku` session rotates through only three modes,
	// manual -> acceptEdits -> plan, with no "auto" at all.
	//
	// Without this the loop spends its entire ceiling pressing, and — far worse
	// — leaves the agent parked in whatever arbitrary mode the last press
	// produced. Rotating an agent's PERMISSION mode somewhere nobody asked for
	// and then returning an error is the one outcome worth extra code to avoid.
	seen := map[domain.AgentMode]bool{current: true}

	maxPresses := domain.ModePressCap(agent.AgentType)
	for change.Presses < maxPresses {
		if change.Mode == want {
			return change, nil
		}
		// Re-proved every iteration: the screen is live, and a modal that
		// appeared since the last press rebinds the chord we are about to send.
		if !domain.ComposerReadyForMode(agent.AgentType, pane) {
			return change, ErrModeUnsafe
		}
		if err := chord.SendChord(ctx, change.PaneID, domain.ShiftTab); err != nil {
			return change, fmt.Errorf("sending shift+tab to %s: %w", change.PaneID, err)
		}
		change.Presses++

		var settled bool
		pane, current, settled, err = a.awaitModeChange(ctx, agent.AgentType, change.PaneID, change.Mode, opts)
		if err != nil {
			return change, err
		}
		// The pane stopped reporting a mode at all — it went modal, or the
		// capture caught it mid-repaint and never recovered inside the settle
		// window. Either way the next press would be blind, so stop here rather
		// than carry a stale mode forward as if it were current.
		if !settled {
			return change, ErrModeUnreadable
		}
		before := change.Mode
		change.Mode = current
		if change.Mode == want {
			return change, nil
		}
		// Only a mode that CHANGED into one already seen means the rotation
		// closed. A mode that did not change at all is a different diagnosis —
		// the chord did not land — and must keep pressing up to the ceiling, or
		// one slow repaint would be reported as "this agent does not offer that
		// mode". The ceiling is what bounds a genuinely deaf agent.
		if change.Mode != before && seen[change.Mode] {
			offered := sortedModes(seen)
			a.restoreMode(ctx, chord, agent.AgentType, &change, opts, maxPresses)
			return change, fmt.Errorf("%s does not offer %s mode — its shift+tab cycle is %s",
				change.Label(target), want, strings.Join(offered, " -> "))
		}
		seen[change.Mode] = true
	}
	if change.Mode == want {
		return change, nil
	}
	a.restoreMode(ctx, chord, agent.AgentType, &change, opts, maxPresses)
	return change, fmt.Errorf("agent %s is still in %s mode after %d shift+tab presses (wanted %s)",
		change.Label(target), change.Mode, change.Presses, want)
}

// restoreMode rotates the agent back to the mode it started in after a failed
// set, so a refusal leaves the agent where the operator left it rather than
// parked wherever the last press landed.
//
// Best-effort by design: it is already running on the failure path, so it must
// never mask the error that got us here, and it never presses into a pane that
// is not showing its composer. It is bounded by the same ceiling — restoring
// costs at most one more rotation — and it stops the moment the pane reports
// the starting mode.
func (a *App) restoreMode(ctx context.Context, chord ports.ChordSender, agentType string, change *ModeChange, opts ModeOptions, maxPresses int) {
	for range maxPresses {
		if change.Mode == change.From {
			return
		}
		pane, err := a.readModePane(ctx, change.PaneID)
		if err != nil || !domain.ComposerReadyForMode(agentType, pane) {
			return
		}
		if err := chord.SendChord(ctx, change.PaneID, domain.ShiftTab); err != nil {
			return
		}
		change.Presses++
		_, mode, settled, err := a.awaitModeChange(ctx, agentType, change.PaneID, change.Mode, opts)
		if err != nil || !settled {
			return
		}
		change.Mode = mode
	}
}

// sortedModes renders an observed cycle deterministically for an error message.
func sortedModes(seen map[domain.AgentMode]bool) []string {
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, string(m))
	}
	sort.Strings(out)
	return out
}

// awaitModeChange polls the pane until the reported mode differs from prev, and
// returns the capture it decided on along with that mode.
//
// Waiting for the CHANGE rather than sleeping a fixed delay is what keeps the
// loop from overshooting: press, fixed-sleep, press would send a second chord
// into a pane that had merely not repainted yet, and on Codex's two-mode toggle
// two presses land exactly back where they started — an infinite loop that
// reports "still in default mode" while flipping the agent twice per iteration.
//
// A timeout is NOT an error. The mode may genuinely not have moved (the chord
// did not land), and the caller's press ceiling is what bounds that; returning
// the last capture lets the next iteration re-check composer readiness against
// fresh content.
//
// The returned capture and mode ALWAYS describe the same read, and ok reports
// whether that read parsed at all. Letting them drift apart was a real hazard:
// carrying the previous mode forward next to a capture that no longer shows one
// would report the agent as still being in a mode the pane no longer claims, and
// the caller would then decide its next press against that stale pairing.
// The deadline uses the real clock, NOT App.now(): this loop's other half is a
// real time.Timer in sleepCtx, and a test injecting a frozen App.Clock would
// then hold the deadline still while the sleeps advanced — spinning here,
// re-reading the pane every poll interval until the context died.
func (a *App) awaitModeChange(ctx context.Context, agentType, paneID string, prev domain.AgentMode, opts ModeOptions) (pane string, mode domain.AgentMode, ok bool, err error) {
	deadline := time.Now().Add(opts.settleTimeout())
	for {
		if err := sleepCtx(ctx, opts.pollInterval()); err != nil {
			return pane, mode, ok, err
		}
		current, err := a.readModePane(ctx, paneID)
		if err != nil {
			return pane, mode, ok, fmt.Errorf("reading pane %s: %w", paneID, err)
		}
		pane = current
		mode, ok = domain.AgentModeFromPane(agentType, current)
		if ok && mode != prev {
			return pane, mode, true, nil
		}
		if !time.Now().Before(deadline) {
			return pane, mode, ok, nil
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func joinModes(modes []domain.AgentMode) string {
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		out = append(out, string(m))
	}
	return strings.Join(out, ", ")
}

// modeFillBudget bounds ONE FillAgentModes pass end to end, for the same reason
// cwdFillBudget bounds FillAgentCwds: each read is its own subprocess with a 15s
// CLI budget and they run in sequence. A displayed mode is a nicety — whatever
// resolves in the budget is what shows.
const modeFillBudget = 3 * time.Second

// FillAgentModes populates st.AgentModes for the agents in st.MonitoredAgents,
// or for just the agent ids named in only.
//
// Deliberately NOT part of GetStatus, and deliberately NOT cached: unlike a cwd,
// the mode is exactly the thing an operator flips by hand mid-session, so a
// stale reading is worse than a blank one. That makes the fill EXPENSIVE — one
// `herdr pane read` subprocess per agent, every call — so callers on a repeating
// tick must narrow it with `only` rather than paying for agents whose mode is
// not on screen. The TUI passes the one agent whose detail view is open; a
// one-shot `hap agents` fills them all.
//
// Best-effort throughout — an unreadable pane is left out of the map, and the
// caller shows a blank.
func (a *App) FillAgentModes(ctx context.Context, st *Status, only ...string) {
	if a.Herdr == nil {
		return
	}
	if _, ok := a.Herdr.(ports.VisiblePaneReader); !ok {
		return
	}
	var wanted map[string]bool
	if len(only) > 0 {
		wanted = make(map[string]bool, len(only))
		for _, id := range only {
			wanted[id] = true
		}
	}
	ctx, cancel := context.WithTimeout(ctx, modeFillBudget)
	defer cancel()

	for _, agent := range st.MonitoredAgents {
		if ctx.Err() != nil {
			return // budget spent: keep whatever resolved
		}
		if wanted != nil && !wanted[agent.AgentID] {
			continue
		}
		if domain.AgentModesFor(agent.AgentType) == nil {
			continue
		}
		pane, err := a.readModePane(ctx, panePreferred(agent))
		if err != nil {
			continue
		}
		mode, ok := domain.AgentModeFromPane(agent.AgentType, pane)
		if !ok {
			continue
		}
		if st.AgentModes == nil {
			st.AgentModes = map[string]domain.AgentMode{}
		}
		st.AgentModes[agent.AgentID] = mode
	}
}
