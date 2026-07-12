# Requirements Delta: Herd Auto Prompter TUI UI/UX

> Code references (`tui.go:<line>` etc.) verified against commit `6a22f8a` (main, 2026-07-11), which includes the per-entry Current situation change (#29) that shifted references below tui.go ~880 by +6 versus earlier revisions of this spec.

## Overview

This is a delta specification for a focused-polish enhancement to the Herd Auto Prompter TUI (`internal/tui`). Each requirement is coded:

- **AR-** = Add (new behavior) — carries Description, User Role, Acceptance Criteria.
- **CR-** = Change (modify existing behavior) — carries Description, User Role, and Acceptance Criteria in which each `~~struck-through~~` original criterion is immediately followed by its replacement(s).
- **BR-** = Remove (retire existing behavior) — carries Description and a `Reference: file:line`.

Requirements use EARS+ syntax and are grouped by impacted module. Scope is TUI presentation only: no `frontend.App` method signature changes, no CLI-parity behavior changes (FR-022), no existing keybinding is reassigned, and the incumbent Bubble Tea + lipgloss stack is retained.

Terminology: a **list tab** is one of Agents, Escalations, Audit, or Rules — the four tabs that render a scrollable row list (Config and Pause/Kill are not list tabs and are out of scope for list scrolling/search). `pageSize` is the number of list rows that fit under the current pane chrome, computed the same way `detailPageSize()` already computes the detail-overlay page size.

## Impacted User Roles

- **Operator** — the single human who runs the TUI as a Herdr pane and drives every control (navigation, search, resolving escalations, editing config, pause/kill). This is the only user role for the TUI. This change introduces no new user role; every AR/CR below applies to the Operator.

---

## Module: `internal/tui` — List viewport & scrolling

### AR-001 — Per-list viewport offset
**Description:** Each list tab tracks the index of its first visible row so the list body can be windowed.
**User Role:** Operator
**Acceptance Criteria:**
- The system SHALL maintain a viewport `offset` (index of the first visible row) for each list tab, initialized to 0.

### AR-002 — Windowed list rendering
**Description:** A list tab renders only the slice of rows that fits the pane.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN rendering a list tab, the system SHALL display only rows `[offset, offset + pageSize)` of that tab's currently visible (filtered) rows.

### AR-003 — Offset follows the shared cursor when scrolling down
**Description:** Moving the selection below the window scrolls the window down to keep it visible.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN the shared `m.cursor` moves to a row at or beyond `offset + pageSize`, the system SHALL increase `offset` so the cursor row remains visible within the window.

### AR-004 — Offset follows the shared cursor when scrolling up
**Description:** Moving the selection above the window scrolls the window up to keep it visible.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN the shared `m.cursor` moves to a row before `offset`, the system SHALL decrease `offset` so the cursor row remains visible within the window.

### CR-005 — Shared cursor retained, not made per-tab
**Description:** The existing single shared list cursor is preserved; only the new per-tab offset is added alongside it.
**User Role:** Operator
**Acceptance Criteria:**
- ~~The system uses a single shared `m.cursor` for list selection (tui.go 366–372) with no viewport offset.~~
- The system SHALL continue to use the single shared `m.cursor` for list selection and SHALL NOT introduce per-tab cursor state; the new per-tab `offset` is the only added selection-related state.

### CR-006 — Offset reset on tab switch
**Description:** Switching tabs resets the viewport alongside the existing cursor reset.
**User Role:** Operator
**Acceptance Criteria:**
- ~~On tab switch the system resets `m.cursor` to 0 (tui.go 297/302/359/363).~~
- WHEN the operator switches tabs, the system SHALL reset both `m.cursor` and that tab's viewport `offset` to 0.

### CR-007 — Offset clamped on resize
**Description:** Resizing the pane keeps the viewport within valid bounds.
**User Role:** Operator
**Acceptance Criteria:**
- ~~On `tea.WindowSizeMsg` the system clamps only `detail.offset` (tui.go 226).~~
- WHEN a `tea.WindowSizeMsg` is received, the system SHALL clamp each list tab's `offset` to `[0, max(0, rowCount − pageSize)]`, consistent with the existing `detail.offset` clamp.

### CR-008 — Cursor clamp accounts for filtered row count
**Description:** The cursor never points past the currently visible (filtered) list.
**User Role:** Operator
**Acceptance Criteria:**
- ~~The system clamps `m.cursor` to the unfiltered row count (tui.go 232–233), where `rowCount()` (tui.go 463–479) returns raw slice lengths for the Agents, Escalations, and Audit tabs (only `tabSignatures` reflects `sigMode` via `visibleSignatures()`).~~
- WHEN clamping `m.cursor`, the system SHALL clamp against the count of currently visible (filtered) rows so the cursor never points past the filtered list.
- The system SHALL make `rowCount()` (tui.go 463–479) search-filter-aware for every list tab (Agents, Escalations, Audit, Rules) so the cursor-clamp path on refresh (tui.go 232–233) and the resize clamp (CR-007) both operate on the filtered count; the Rules tab SHALL continue to compose its `sigMode` filter (per CR-017) with the search query.

### AR-009 — Clipped-rows affordance
**Description:** A "N more" indicator shows when rows exist below the window.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN the visible (filtered) row count exceeds `offset + pageSize`, the system SHALL render a "… N more" indicator showing how many rows remain below the window, consistent with the existing detail-overlay "… N more line(s)" affordance (tui.go 1291).

### AR-010 — Render within pane height
**Description:** A list tab never overflows the pane, preserving the height invariant already enforced for the detail view.
**User Role:** Operator
**Acceptance Criteria:**
- WHILE rendering any list tab, the system SHALL keep the header, windowed rows, and footer/help within the current pane height so no row or the help line is clipped.

### CR-032 — Rules tab shows the full signature ID
**Description:** The Rules tab list renders each rule's complete signature ID (e.g. `idle:83b2285833fd4c6ebf40a2bc`) instead of the abbreviated form, so the operator can read the full identifier without opening the detail overlay.
**User Role:** Operator
**Acceptance Criteria:**
- ~~The Rules tab renders each row's signature ID via `shortSig()`, which truncates it to 16 characters plus an ellipsis in a fixed 18-cell column (`shortSig` tui.go 1038–1043; `renderSignatures` tui.go 1266–1269).~~
- WHEN rendering the Rules tab list, the system SHALL display each row's full signature ID untruncated (e.g. `idle:83b2285833fd4c6ebf40a2bc`, not `idle:83b2285833f…`), and SHALL NOT abbreviate it via `shortSig()`.
- The system SHALL size the signature-ID column to the widest full ID among the currently visible (filtered) rows so the remaining columns shift right without overlapping, consistent with the existing fixed-column row layout in `renderSignatures`.
- The change SHALL apply to the Rules list body only and SHALL NOT alter the abbreviated `shortSig()` rendering used in the signature detail-overlay title (tui.go 268) or the delete-confirmation prompt (tui.go 975, 986).

---

## Module: `internal/tui` — In-list search / filter

### AR-011 — Search entry key
**Description:** Pressing the currently-unbound `/` opens search for the active list tab.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN the operator presses `/` on the Agents, Escalations, Audit, or Rules tab, the system SHALL enter search-input mode for that tab.

### AR-012 — Incremental query editing
**Description:** Typing edits the query and re-filters immediately.
**User Role:** Operator
**Acceptance Criteria:**
- WHILE in search-input mode, the system SHALL append typed characters to the tab's query, remove the last character on backspace, and recompute the filtered rows after each edit.

### AR-013 — Filter predicate
**Description:** A non-empty query narrows the list by case-insensitive substring match.
**User Role:** Operator
**Acceptance Criteria:**
- WHILE a tab's search query is non-empty, the system SHALL display only rows whose visible column text contains the query as a case-insensitive substring.

### AR-014 — Exit search mode retaining the active filter
**Description:** Leaving search-input mode keeps the current query as an active filter.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN the operator presses `esc` while in search-input mode with a non-empty query, the system SHALL exit search-input mode and retain the current query as an active filter over the list.
- WHEN the operator presses `enter` while in search-input mode, the system SHALL behave identically to `esc`: exit search-input mode retaining a non-empty query as the active filter (an empty query leaves no filter), and SHALL NOT trigger the tab's `enter` action (confirm/detail/edit).

### AR-015 — Clear the active filter
**Description:** The operator can restore the full list without a new keybinding, using the query-editing path already defined.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN the operator deletes the query back to empty via backspace (per AR-012), the system SHALL clear the filter and restore the full list.
- WHEN the operator presses `esc` while in search-input mode with an already-empty query, the system SHALL exit search-input mode with no active filter.
- WHILE on a list tab that is NOT in search-input mode and has no active filter, the system SHALL treat `esc` as a no-op, preserving today's behavior where the top-level list-mode key router has no `esc` handler; `esc` SHALL retain its existing meaning of closing the detail overlay (tui.go 314) and cancelling an inline prompt. This adds a list-mode meaning to `esc` only while search is engaged and reassigns no existing binding.

### CR-016 — Search recomputes cursor and offset
**Description:** Changing the filter keeps selection and viewport within bounds.
**User Role:** Operator
**Acceptance Criteria:**
- ~~Cursor and offset are recomputed only on tab switch and resize.~~
- WHEN the filtered row set changes due to a query edit, the system SHALL clamp the shared `m.cursor` and the tab's `offset` to the new filtered row count so both stay within bounds.

### CR-017 — Rules mode filter preserved and composed with search
**Description:** The existing Rules `f`-key mode cycle keeps working and combines with the new search.
**User Role:** Operator
**Acceptance Criteria:**
- ~~The Rules tab filters solely by `sigMode` via the `f` key (`visibleSignatures()`, tui.go 124–136, 405).~~
- The system SHALL retain the existing Rules `f`-key mode cycle and SHALL compose it with the new search query so both filters apply together on the Rules tab.

### AR-018 — Empty-result feedback
**Description:** A filter that matches nothing shows an explicit empty-state message.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN a tab's active filter matches zero rows, the system SHALL render an explicit empty-state message indicating no rows match the current filter, consistent with the existing Rules "no signatures match the filter" message (tui.go 1256).

### CR-019 — Search keys do not trigger action bindings
**Description:** Typing a query never fires a destructive or navigational action binding.
**User Role:** Operator
**Acceptance Criteria:**
- ~~Printable keys (`q`, `y`, `p`, `r`, `e`, `c`, `n`, `v`, `f`, `a`, `t`, `x`, `X`, `j`, `k`, `h`, `l`, space) always invoke their action or navigation bindings — including `q` (quit, tui.go 355), `y` (confirm+send on Escalations, tui.go 379), `j`/`k` (cursor), and `h`/`l` (tab switch).~~
- WHILE in search-input mode, the system SHALL route ALL printable keys to the query and SHALL NOT invoke any action or navigation binding — in particular `q` (quit), `y` (confirm+send), `p`, `r`, `e`, `c`, `n`, `v`, `f`, `a`, `t`, `x`, `X`, `j`/`k` (cursor), `h`/`l` (tab switch), and space.

---

## Module: `internal/tui` — Theming & palette

### CR-020 — Centralized palette
**Description:** All styles resolve through one named palette instead of scattered color literals.
**User Role:** Operator
**Acceptance Criteria:**
- ~~Styles use hardcoded `lipgloss.Color` literals (titleStyle 205, errStyle 196, okStyle 46, sectionStyle 117, pausedStyle 196, runningStyle 46 at tui.go 41–50).~~
- The system SHALL resolve every list/detail/status style through a single named palette rather than scattered hardcoded `lipgloss.Color` literals.

### AR-021 — Named theme selection
**Description:** The active palette is chosen by name from config.
**User Role:** Operator
**Acceptance Criteria:**
- The system SHALL select the active palette from the `[tui] theme` configuration value, supporting at least the named themes `default`, `dark`, `light`, and `high-contrast`.

### AR-022 — `default` and empty resolve to the current appearance
**Description:** There is exactly one "current look" palette, reachable either by omitting `theme` or by naming it `default`.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN `[tui] theme` is empty/absent OR set to `default`, the system SHALL resolve to the identical current-appearance palette using the exact 256-color values in use today, so `default` is the explicit spelling of the empty-string fallback and existing operators observe no visual change.

### AR-023 — Optional per-role overrides
**Description:** Individual color roles can be overridden on top of the selected theme.
**User Role:** Operator
**Acceptance Criteria:**
- WHERE per-role color overrides are provided in `[tui]`, the system SHALL apply them on top of the selected named theme, and an unset role SHALL inherit the named theme's value.

### CR-024 — Empty / loading / error state styling
**Description:** Empty, loading, and error states render through the resolved palette.
**User Role:** Operator
**Acceptance Criteria:**
- ~~Error state renders via the hardcoded `errStyle` (`errStyle.Render("error: …")`, tui.go 1192).~~
- The system SHALL style empty-list, pre-first-data (loading), and error states through the resolved palette, retaining the existing error rendering path via the palette's error role.

---

## Module: `internal/tui` — Action feedback / status area

### CR-025 — Durable structured status area
**Description:** Action outcomes persist in a clearly-placed status area rather than a single easily-missed line.
**User Role:** Operator
**Acceptance Criteria:**
- ~~Action outcomes surface only as a single transient `m.message` line.~~
- WHEN an action produces an outcome, the system SHALL present it in a durable, clearly-placed status area so the outcome of a multi-step operation remains readable after it completes.

### CR-026 — Success/error differentiation retained
**Description:** Success and error outcomes stay visually distinct, now sourced from the palette.
**User Role:** Operator
**Acceptance Criteria:**
- ~~Success renders via `okStyle` and errors via `errStyle` from hardcoded literals (tui.go 251–253).~~
- The system SHALL render success outcomes via the palette's ok role and errors via the palette's error role, preserving today's success-versus-error differentiation.

---

## Module: `internal/config` — TUI configuration

### AR-027 — `theme` field
**Description:** A new optional `theme` key selects the palette.
**User Role:** Operator
**Acceptance Criteria:**
- The system SHALL add an optional `theme` string field to the `[tui]` TOML table (`config.TUI`), defaulting to empty (which AR-022 resolves to the current appearance).

### AR-028 — `palette` override field
**Description:** A new optional per-role override mechanism layers on the selected theme.
**User Role:** Operator
**Acceptance Criteria:**
- The system SHALL add an optional per-role color override mechanism to the `[tui]` TOML table (`config.TUI`), WHERE absent roles inherit the selected named theme (per AR-023).

### CR-029 — Backward-compatible config
**Description:** Existing `[tui]` config files stay valid with the new optional fields.
**User Role:** Operator
**Acceptance Criteria:**
- ~~`[tui]` exposes only `MaxContentWidth`.~~
- The system SHALL keep existing `[tui]` configuration files valid, leave `MaxContentWidth` behavior unchanged, and treat all new theme fields as optional with safe defaults via `config.Default()`.

### AR-030 — Invalid theme handling
**Description:** An unknown theme name degrades gracefully to the default.
**User Role:** Operator
**Acceptance Criteria:**
- IF `[tui] theme` names an unknown theme, THEN the system SHALL fall back to the default palette rather than failing to start.

---

## Module: `internal/tui` + `internal/frontend` — Config tab completeness

### CR-033 — Config tab exposes every scalar config field
**Description:** The Config tab's editable field list (shared with `config set`) covers the scalar fields added to the config since the registry was last extended, plus the new `[tui]` keys this spec introduces.
**User Role:** Operator
**Acceptance Criteria:**
- ~~`ConfigFieldKeys` (frontend.go 450–471) lists 20 scalar keys and omits `llm.pane_excerpt_chars` (config.go 83), `safety.disable_seed` (config.go 44), and `tui.max_content_width` (config.go 162), so the Config tab (`buildRuleItems`, tui.go 184–217) and `config set` neither display nor edit them.~~
- The system SHALL add `llm.pane_excerpt_chars`, `safety.disable_seed`, and `tui.max_content_width` to `ConfigFieldKeys`, `FieldValue` (frontend.go 474–521), and `SetField` (frontend.go 525–), with type-appropriate validation, so each is displayed and editable on the Config tab and via `config set` alike (CLI parity, FR-022); all four new keys are simple scalars and therefore TUI-editable per CR-036's classification.
- WHEN the `tui.theme` field lands (AR-027), the system SHALL expose it through the same registry, with `SetField` validation consistent with AR-030 (unknown names fall back to the default palette, never an error that blocks saving other fields — or reject with a clear message listing the valid names; pick ONE behavior and test it).
- The system SHALL keep `ConfigFieldKeys`, `FieldValue`, and `SetField` in three-way parity, and a unit test SHALL fail when a key is present in one and missing in another (extending the existing iteration test at frontend_test.go 379).

### AR-034 — Read-only display of safety-indicator configuration
**Description:** The Config tab shows the operator the suspected-irreversible indicator patterns that are in effect, which today are invisible outside `config.toml`.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN rendering the Config tab, the system SHALL display `safety.irreversible_indicators` and each `[[safety.indicator_rules]]` entry (pattern + agent scope; config.go 48–59) as read-only rows in a "Safety indicators" section, following the existing sectioned layout of `buildRuleItems` (tui.go 184–217).
- WHEN the operator invokes edit/remove (`enter`/`e`/`x`) on one of these rows, the system SHALL show an informational message that they are edited in `config.toml` (mirroring the existing "config fields are edited (enter), not removed" message, tui.go 1140) and SHALL NOT mutate config.

### AR-035 — Read-only display of capture-delay rules
**Description:** The Config tab shows the effective delayed-capture timing, including the built-in defaults when no rule is configured.
**User Role:** Operator
**Acceptance Criteria:**
- WHEN rendering the Config tab, the system SHALL display each `[[capture_delay]]` rule (agent type, start_ms, event_ms; config.go 151–155) as a read-only row.
- WHEN no `[[capture_delay]]` rules are configured, the system SHALL show the effective built-in defaults (start 10000 ms / event 500 ms) so the operator can see what timing applies.
- Classifier manifests (`[[classifier]]`, config.go 141–146) SHALL remain file-only and out of scope for this change.

### CR-036 — TUI field editability classification
**Description:** Only simple single-value fields stay editable through the TUI's inline prompt; free-text and argv-template fields become TUI-read-only, because their one-line prompt round-trip mangles real values. `config set` is unaffected.
**User Role:** Operator
**Acceptance Criteria:**
- ~~Every `ConfigFieldKeys` row is editable through the inline prompt (`editSelectedRule`, tui.go 1054–1074) — including the argv-template fields `llm.command` and `llm.rewrite_command`, whose edit path round-trips a one-line quoting parse (`JoinCommand` display → hand-retyped prompt → `SplitCommand`, frontend.go 627–) that mangles values containing nested quotes or JSON arguments, so editing them in the TUI is effectively broken.~~
- The system SHALL classify each registry field as TUI-editable or TUI-read-only. Numeric, boolean, and short-enum fields SHALL remain TUI-editable: `thresholds.*`, `learning.graduation_n`, `limits.*`, `llm.timeout_seconds`, `llm.auto_act`, `llm.rewrite_timeout_seconds`, `llm.pane_excerpt_chars`, `safety.disable_seed`, `embedding.disabled`, `embedding.similarity_threshold`, `embedding.bm25_min_score`, `embedding.gpu_layers`, `tui.max_content_width`, `tui.theme`.
- The system SHALL mark the free-text/argv fields TUI-read-only: `llm.command`, `llm.rewrite_command`, `llm.rewrite_fallback_template`, `embedding.model_path`.
- The system SHALL apply this as a general rule, not a fixed list: any registry field whose value is free-form text — argv templates, template strings with placeholders, file paths, or any other long string — SHALL default to TUI-read-only when added, now or in the future; only single-value numeric, boolean, and short-enum fields qualify as TUI-editable. The classification SHALL live in one place in the registry (e.g. a per-key editable flag or type tag) so a new field cannot become TUI-editable by omission.
- WHERE per-role palette overrides exist in `[tui]` (AR-028), they SHALL follow the same rule: visible on the Config tab at most as read-only rows, edited only via `config.toml`; `tui.theme` (a short enum) remains the only TUI-editable theming key.
- WHEN the operator presses `enter` or `e` on a TUI-read-only field, the system SHALL show an informational message directing them to edit the value in `config.toml` (noting that `config set <key> <value>` also remains available) and SHALL NOT open the edit prompt or mutate config.
- The system SHALL keep `SetField` accepting ALL registry keys unchanged — the read-only classification applies to the TUI inline prompt only, preserving CLI parity (FR-022).

### CR-037 — Truncated display of long config values
**Description:** Long field values render truncated to one line instead of overflowing the row.
**User Role:** Operator
**Acceptance Criteria:**
- ~~Config rows render the full `FieldValue` string (`buildRuleItems` label `"%-38s %s"`, tui.go 189), so a long `llm.command` or `llm.rewrite_command` overflows or wraps the row.~~
- WHEN a config row's rendered value exceeds the row's width budget, the system SHALL truncate it with a trailing ellipsis (consistent with the existing `oneLine` helper, tui.go 1452) so every config row stays on one line; the full value remains available in `config.toml`.

---

## Module: `internal/frontend` — Non-impact assertion

### AR-031 — No frontend API change
**Description:** All changes are presentation-only; the frontend API is untouched apart from the additive field-registry entries CR-033 requires.
**User Role:** Operator
**Acceptance Criteria:**
- The system SHALL implement all of the above without changing any `frontend.App` method signature or returned data shape, and the TUI SHALL continue to obtain its data through the existing `App` methods (`GetStatus`, `Escalations`, `Audit`, `Signatures`, etc.).
- WHERE CR-033 extends the field registry, the frontend change SHALL be limited to additive entries in `ConfigFieldKeys`, `FieldValue`, and `SetField` — no signature or returned-shape change, and every existing key keeps its current behavior.

---

## Module: `internal/tui` — Non-list tab non-impact assertion

### AR-032 — Config and Pause/Kill navigation unchanged
**Description:** The scrolling, search, and filtered-`rowCount()` changes leave the non-list tabs' cursor navigation intact.
**User Role:** Operator
**Acceptance Criteria:**
- The system SHALL NOT apply list scrolling, search-input mode, or the search filter to the Config or Pause/Kill tabs, which share the single `m.cursor` and are driven by `rowCount()` (Config via `len(m.items)`/`buildRuleItems`, Pause/Kill via `len(m.data.kills)`, tui.go 463–479).
- WHILE making `rowCount()` search-filter-aware for the four list tabs (per CR-008), the system SHALL leave the `tabConfig` and `tabKill` branches of `rowCount()` returning their current counts so Config field/pattern/source navigation and the Pause/Kill view behave exactly as they do today.
