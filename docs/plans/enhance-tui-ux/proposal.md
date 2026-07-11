# Proposal: Enhance the Herd Auto Prompter TUI UI/UX

> Code references (`tui.go:<line>` etc.) verified against commit `6a22f8a` (main, 2026-07-11).

## Why

The TUI (`internal/tui`) is the primary control surface for Herd Auto Prompter, mirroring every CLI capability across six tabs (Agents, Escalations, Audit, Rules, Config, Pause/Kill). It is functional but has accumulated UX debt that degrades operability as real deployments grow:

- **List views do not scroll or paginate.** Scrolling exists only inside the detail overlay (`detailView` uses `m.detail.offset` / `detailPageSize()`). The list bodies for Agents, Escalations, Audit, and Rules render from the top with no viewport offset, so once the number of rows exceeds the available pane height the tail of the list is silently clipped. On a busy herd (many monitored agents, a backlog of escalations, a long audit trail), the operator cannot reach rows below the fold — a correctness-affecting gap, not just cosmetics.
- **Filtering is inconsistent and absent from most tabs.** Only the Rules tab has a display-side filter (the `f` key cycling `sigMode` via `visibleSignatures()`). Agents, Escalations, and Audit offer no way to narrow a long list, so finding a specific agent or record means visual scanning.
- **Theming is hardcoded and not operator-controllable.** Colors are literal 256-color codes scattered through package-level `lipgloss.NewStyle()` vars (e.g. `titleStyle` `Color("205")`, `errStyle` `Color("196")`, `okStyle` `Color("46")`). There is no palette abstraction and the `[tui]` config exposes only `MaxContentWidth`. Operators on light terminals, low-color terminals, or with accessibility needs cannot adjust contrast.
- **The Config tab lags the config surface.** The tab's editable field list is the fixed `frontend.ConfigFieldKeys` registry (frontend.go 450–471), which omits recently added scalar fields — `llm.pane_excerpt_chars` (config.go 83), `safety.disable_seed` (config.go 44), and `tui.max_content_width` (config.go 162) — and the tab has no display at all for the structured safety-indicator configuration (`safety.irreversible_indicators` / `[[safety.indicator_rules]]`, config.go 48–59) or `[[capture_delay]]` rules (config.go 151–155). The operator cannot see or edit these without opening `config.toml`, and nothing keeps the registry in sync as config grows. Meanwhile the argv-template fields that ARE editable (`llm.command`, `llm.rewrite_command`) round-trip through a one-line quoting parse (`JoinCommand` display → hand-retyped prompt → `SplitCommand`, frontend.go 627–) that mangles real-world values with nested quotes or JSON arguments — editing them inline is effectively broken — and their full values overflow the config row (`buildRuleItems` renders `FieldValue` untruncated, tui.go 189).
- **Action feedback is transient and unstructured.** Outcomes surface through a single status string (`m.message`). Color differentiation already exists — success is rendered with `okStyle` and errors with `errStyle` (tui.go lines 251–253) — so the gap is *not* first-time color coding. The gap is persistence, structure, and placement: the message is a single transient line that is easily missed after a multi-step operation (resolve, rename, delete). The delta is a durable, structured status area, not colorizing what is already colorized.

This is a **focused polish** effort: cohesive, incremental UX improvements to the existing six-tab layout — not a redesign.

## What Changes

Scoped to the TUI presentation layer only. In scope:

1. **List scrolling & pagination** — introduce a per-list viewport offset (mirroring the detail overlay's proven `offset` + page-size model) for the Agents, Escalations, Audit, and Rules list bodies, with keyboard scrolling, selection that keeps the cursor in view, and a "N more" affordance when rows are clipped.
2. **In-list search / filter across tabs** — generalize the Rules-only filter into a consistent search/filter interaction available on Agents, Escalations, Audit, and Rules (incremental text match over the visible columns), preserving the existing Rules mode-cycle behavior.
3. **Configurable visual theming** — centralize the hardcoded color literals into a single named palette, add theme fields to the existing `[tui]` TOML section (with safe defaults preserving today's look), and improve empty / loading / error state rendering.
4. **Improved action feedback** — evolve the single transient status line into a durable, structured status area. The existing `okStyle`/`errStyle` success-vs-error color differentiation is retained; the new work adds persistence and clear placement so outcomes of multi-step operations are not missed.
5. **Config tab completeness** — extend the scalar field registry (`ConfigFieldKeys` / `FieldValue` / `SetField`) with the missing fields (`llm.pane_excerpt_chars`, `safety.disable_seed`, `tui.max_content_width`, plus the new `tui.theme` from item 3) so they are displayed and editable on the Config tab and via `config set` alike (CLI parity, FR-022), and add read-only Config-tab sections for the safety-indicator patterns and capture-delay rules (with effective built-in defaults shown when unconfigured). Field rows gain an editability classification: only simple single-value fields (numbers, booleans, the `tui.theme` enum) stay editable through the TUI prompt; free-text fields — argv templates, template strings, paths, and any other long string, today `llm.command`, `llm.rewrite_command`, `llm.rewrite_fallback_template`, `embedding.model_path` — become TUI-read-only as a standing rule (new free-text fields default to read-only) — `enter`/`e` shows a message directing the operator to `config.toml`, while `config set` continues to accept every key — and long values render truncated to one line. Classifier manifests (`[[classifier]]`) remain file-only.

Explicitly **out of scope** (per interview constraints):

- No new TUI framework — stays within Bubble Tea + lipgloss.
- No changes to the `frontend.App` API surface or the CLI-parity contract (FR-022). These are presentation-only changes; the TUI keeps calling the same `App` methods.
- No reassignment of existing keybindings — new keys are additive only.
- No navigation/layout restructure (no side panel, no dashboard) — the six-tab model is preserved.

## Technical Solution

### Library / framework changes

None. All work uses the incumbent stack: `charmbracelet/bubbletea` (Model/Update/View loop) and `charmbracelet/lipgloss` (styling). No dependency is added, removed, or upgraded.

### Core-flow changes — list rendering & input

Today only the detail overlay maintains a scroll offset. The list tabs share a single `m.cursor` (up/down handling at tui.go lines 366–372, clamped on refresh (`refreshMsg`) at 232–233 — the `WindowSizeMsg` handler clamps only `detail.offset`, at 226 — and reset to 0 on tab switch at 297/302/359/363). The design **keeps that single shared `m.cursor`** — cursor state does not become per-tab — and adds a viewport `offset` that *follows* the cursor to keep it visible. The `offset` resets to 0 alongside the existing cursor reset on tab switch, and is clamped on `WindowSizeMsg` exactly as `detail.offset` already is. The updated per-tab render/input flow:

```
tea.KeyMsg
  │
  ├─ search-input active? ── yes ─▶ append/backspace into query ──▶ recompute filtered rows
  │                                                                └─▶ clamp shared cursor + viewport offset
  │
  ├─ ↑ / k ─▶ move shared m.cursor up ──▶ if cursor < offset: offset-- (keep cursor visible)
  ├─ ↓ / j ─▶ move shared m.cursor down ▶ if cursor ≥ offset+pageSize: offset++
  ├─ /      ─▶ enter search-input mode for the active tab
  ├─ f      ─▶ (Rules only) cycle sigMode — unchanged
  └─ other  ─▶ existing handlers (enter/v details, x delete, resolve, …)

View(activeTab):
  rows      = filter(allRows, query, mode)      # search + existing Rules mode
  pageSize  = listPageSize(m.height)            # same chrome accounting as detailPageSize
  window    = rows[offset : offset+pageSize]
  render header + window + "… N more" footer when len(rows) > offset+pageSize
```

The viewport `offset` and per-tab search query are new UI state on `Model` (analogous to the existing `detail.offset` and `sigMode`). The shared `m.cursor` is unchanged in kind; only its clamping is extended to account for the active filter's row count.

### Data-model / DB-schema changes

No database schema changes. The only persisted-config change is additive fields on the existing `[tui]` TOML table (`config.TUI` struct), which continues to be optional with defaults via `config.Default()`:

```
config.TUI (before)              config.TUI (after)
┌────────────────────┐           ┌──────────────────────────────────────┐
│ MaxContentWidth int│           │ MaxContentWidth int                  │  (unchanged)
└────────────────────┘           │ Theme          string (named enum)   │  (new, default "" = current look)
                                 │ Palette        (optional overrides)  │  (new, secondary)
                                 └──────────────────────────────────────┘
```

Recommended shape (to be pinned in requirements): a **named-theme enum** as the primary knob — `""`/default, `dark`, `light`, `high-contrast` — with optional per-role color overrides (`Palette`) as a secondary mechanism. This keeps validation and default-resolution simple: an empty/absent `Theme` must resolve to today's exact 256-color values, and per-role overrides layer on top of the selected named theme. Existing operators see no visual change unless they opt in.

### API breaking changes

None. No change to `frontend.App` method signatures, no change to the CLI-parity surface, and no reassignment of existing TUI keybindings. New keybindings (search entry, any new scroll aliases) are additive; existing bindings retain their current meaning.

## Impact

- **`internal/tui` (primary):** new per-list viewport `offset` + per-tab search state on `Model` (the shared `m.cursor` stays shared); `Update` gains search-input handling and list scroll routing; per-tab `View` functions windowed and filtered; color literals replaced by palette lookups; status-area rendering reworked for persistence/structure while keeping `okStyle`/`errStyle`. This is where the bulk of the change lands.
- **`internal/config`:** additive fields on the `TUI` struct (`Theme` named enum + optional `Palette` overrides) plus their defaults; documentation of the new `[tui]` keys. No breaking change to existing config files.
- **`internal/frontend`:** additive only — new entries in `ConfigFieldKeys` (frontend.go 450–471), `FieldValue` (474–521), and `SetField` (525–) for the missing scalar fields and the new `[tui]` keys, with type-appropriate validation. No `App` method signature or returned-shape change; `config set` gains the same keys automatically (shared registry).
- **Tests (`internal/tui/tui_test.go`):** extended to cover list scrolling/clamping (paralleling the existing `TestDetailViewScrolls`), search filtering on each tab (paralleling the existing Rules `f`-filter test), theme resolution with defaults, and the "renders within pane height" invariant.
- **Docs:** README / config reference updated for the new `[tui]` theme keys and search keybinding.
- **Operators:** better navigability of long lists and clearer feedback; zero behavior change unless they opt into a theme. No migration required — new config keys are optional.
