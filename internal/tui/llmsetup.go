package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
)

// First-run LLM setup offer.
//
// A fresh hap has every [llm] command unset, so the consult, task
// generation, learning and the rule judge are all off until the operator
// finds the per-key preset picker on the Config tab. When the TUI opens with
// NO llm command configured, it probes for a logged-in claude or codex in the
// background and, if one is ready, offers to install that CLI's default
// recipes into all four keys at once (frontend.ApplyLLMPresetAll). claude is
// preferred over codex.
//
// Four properties are load-bearing:
//
//   - The probe runs only when the first good refresh shows nothing
//     configured: an operator who has set any LLM command never pays for a
//     subprocess spawn, and the probe never runs on the tick.
//   - The offer appears at most ONCE per TUI session, and declining is
//     session-only by design — no config key records it, so a later launch
//     that still has nothing configured asks again.
//   - It never opens over another modal, and never once the operator has
//     pressed a key. The popup arrives asynchronously, and an enter meant
//     for a Config row landing on it would install recipes — one of which
//     (learn) runs with edit permission — unasked. A busy operator gets a
//     one-line hint instead. And because a key pressed just AFTER it draws
//     was aimed at the screen it covered, the popup swallows every key but
//     ctrl+c for llmSetupArmDelay (prompt.armedAt).
//   - The install refuses to overwrite: the apply re-checks each key under
//     the config lock, so a key configured between the offer and the answer
//     is kept.

// llmSetupArmDelay is how long the offer ignores keys after it opens —
// longer than a keystroke already in flight, shorter than reading the label.
// A variable so tests can arm it at once.
var llmSetupArmDelay = time.Second

// llmSetupDecline is the picker's "no" option.
const llmSetupDecline = "not now"

// llmDetectedMsg carries the background probe's result.
type llmDetectedMsg struct {
	results []llm.CLIReadiness
}

// llmSetupState is the session's first-run offer state.
type llmSetupState struct {
	// enabled turns the whole feature on. Set by run (the real `hap tui`), so
	// the hundreds of unit tests that build a Model and feed it a refresh
	// never spawn a real CLI probe.
	enabled bool
	// probing is set once the probe has been launched; it is never reset, so
	// the probe runs at most once per session.
	probing bool
	// offered is set once the offer (popup or hint) has been made.
	offered bool
	// keyPressed is set by any keypress; the popup never opens after one.
	keyPressed bool
	// results is the probe's answer; nil until it lands.
	results []llm.CLIReadiness
}

// maybeStartLLMDetect launches the probe on the first good refresh that shows
// no LLM configured. cfg is the refresh's config, whose [llm] env the probe
// runs under.
func (m *Model) maybeStartLLMDetect(cfg config.Config) tea.Cmd {
	s := &m.llmSetup
	if !s.enabled || s.probing || s.offered {
		return nil
	}
	if !frontend.LLMUnconfigured(cfg) {
		// Decided on the first good refresh: an LLM the operator already
		// configured is never second-guessed later in the session, even if
		// they clear it.
		s.offered = true
		return nil
	}
	s.probing = true
	app, ctx := m.app, m.ctx
	return func() tea.Msg {
		return llmDetectedMsg{results: app.DetectLLMCLIs(ctx, cfg)}
	}
}

// maybeOfferLLMSetup opens the offer once both the probe and a good refresh
// are in, if it is still wanted. Called from both arrivals, since either can
// land first.
func (m *Model) maybeOfferLLMSetup() {
	s := &m.llmSetup
	// m.data.err: a failed refresh carries a zero config, which would read
	// as "nothing configured" — the probe landing then must not offer.
	if s.offered || s.results == nil || !m.initialized || m.data.err != nil || !frontend.LLMUnconfigured(m.data.cfg) {
		return
	}
	var ready []string
	for _, r := range s.results {
		if r.Ready && slices.Contains(frontend.LLMSetupPresetNames, r.Name) {
			ready = append(ready, r.Name)
		}
	}
	if len(ready) == 0 {
		// Nothing usable: no popup, and nothing to nag about either.
		s.offered = true
		return
	}
	if s.keyPressed {
		s.offered = true
		if m.message == "" && m.prompt == nil && m.confirm == nil {
			m.message = fmt.Sprintf("no LLM configured — %s is ready: run `hap config set llm --preset auto` to set it up", ready[0])
		}
		return
	}
	if m.prompt != nil || m.confirm != nil || m.detail != nil || m.searching {
		// Another modal is up; a later refresh retries while no key has
		// been pressed.
		return
	}
	s.offered = true
	m.openLLMSetupPrompt(ready)
}

// openLLMSetupPrompt opens the single-choice picker. ready is in preference
// order, so the highlight starts on the preferred CLI.
func (m *Model) openLLMSetupPrompt(ready []string) {
	app, ctx, wg := m.app, m.ctx, m.inflight
	// Not beginAction: this opens unasked, so it must not wipe a durable
	// status note (the TUI-limit notice, an action error) the operator has
	// not read yet.
	m.openPrompt(&prompt{
		// Short enough for an 80-column pane: the picker truncates its label.
		label:   "No LLM configured — install the default recipes for a ready CLI? (learning edits AUTO.md)",
		options: append(ready, llmSetupDecline),
		armedAt: time.Now().Add(llmSetupArmDelay),
		onSubmit: func(choice string) tea.Cmd {
			if choice == llmSetupDecline {
				return func() tea.Msg {
					return actionResultMsg{message: "LLM left unconfigured — `hap config set llm --preset auto` sets it up later"}
				}
			}
			// Joins m.inflight like every mutation (see do), so quitting
			// right after enter does not exit mid-write.
			if wg != nil {
				wg.Add(1)
			}
			return func() tea.Msg {
				if wg != nil {
					defer wg.Done()
				}
				return applyLLMSetup(ctx, app, choice)
			}
		},
	})
}

// applyLLMSetup installs preset into every unset [llm] key and reports it.
func applyLLMSetup(ctx context.Context, app *frontend.App, preset string) actionResultMsg {
	applied, skipped, reloaded, err := app.ApplyLLMPresetAll(ctx, preset)
	if err != nil {
		return actionResultMsg{err: err}
	}
	what := "all [llm] commands"
	if len(skipped) > 0 {
		what = strings.Join(applied, ", ")
	}
	msg := fmt.Sprintf("%s set to the %s default recipes — edit config.toml to tune them", what, preset)
	if !reloaded {
		msg += " (saved — no daemon running)"
	}
	return actionResultMsg{message: msg}
}
