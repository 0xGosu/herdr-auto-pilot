# Tasks: Enhance the Herd Auto Prompter TUI UI/UX

Implementation checklist grounded in the approved `proposal.md` and `requirements.md` delta. Scope is TUI presentation only — no `frontend.App` signature changes, no keybinding reassignment, no new framework. `_Requirements:_` codes reference `requirements.md`.

> Code references (`tui.go:<line>` etc.) verified against commit `6a22f8a` (main, 2026-07-11).

> Note: task 30 realizes a new requirement, **CR-032 (Rules list shows the full signature id)**, added to the delta during tasks review at the operator's request. It changes the Rules list column from the truncated `shortSig()` form (`sig[:16] + "…"`, tui.go 1038–1043, used at 1267) to the full `r.Signature` (e.g. `idle:83b2285833fd4c6ebf40a2bc`), which is already carried on the row and shown in the detail overlay (tui.go 996).

> Note: workstream G (tasks 31–34) realizes **CR-033/AR-034/AR-035/CR-036/CR-037 (Config tab completeness & editability)**, added after the operator found recently added config fields missing from the Config tab and inline editing of the argv-template fields (`llm.command`, `llm.rewrite_command`) broken. The finalization tasks were renumbered 31–32 → 35–36 accordingly.

## Dependency Graph

Critical path: **D → C** (config fields before theme resolution) and **A → B** (viewport before search-driven clamping). Workstreams A, C/D, and E are largely independent and can proceed in parallel; B depends on A; C depends on D.

- Foundation: 1, 25 (state + config fields)
- Then: A (2–8), D→C (25–26→18–24), E (27–29), the standalone Rules-id change (30), and G (31–34; 31 waits for 25)
- B (9–17) starts once A tasks 1–6 land
- Finalization: 35–36 last

## Workstream A — List viewport & scrolling

- [ ] 1\. Add per-tab viewport offset state and page-size helper
  - Add a per-tab `offset` field to `Model` and a `listPageSize(m.height)` helper mirroring `detailPageSize()` chrome accounting.
  - Acceptance Criteria:
    - `Model` holds an `offset` per list tab, initialized to 0.
    - `listPageSize` returns the row count that fits under the current pane chrome.
  - _Dependencies: none_
  - _Requirements: AR-001, AR-002_
  - _Complexity: Small_

- [ ] 2\. Window each list tab's View to the offset slice
  - Render only rows `[offset, offset+pageSize)` of the visible rows; keep the shared `m.cursor` unchanged in kind.
  - Acceptance Criteria:
    - Each list tab renders at most `pageSize` rows starting at `offset`.
    - No per-tab cursor state is introduced.
  - _Dependencies: 1_
  - _Requirements: AR-002, CR-005_
  - _Complexity: Medium_

- [ ] 3\. Make the offset follow the shared cursor
  - Route ↑/↓ and `j`/`k` so `offset` increases when cursor ≥ `offset+pageSize` and decreases when cursor < `offset`.
  - Acceptance Criteria:
    - Selecting a row below the window scrolls down; above the window scrolls up; cursor always stays visible.
  - _Dependencies: 1, 2_
  - _Requirements: AR-003, AR-004_
  - _Complexity: Medium_

- [ ] 4\. Reset offset on tab switch
  - At the existing cursor-reset points (tui.go 297/302/359/363), also reset that tab's `offset` to 0.
  - Acceptance Criteria:
    - Switching tabs resets both `m.cursor` and `offset` to 0.
  - _Dependencies: 1_
  - _Requirements: CR-006_
  - _Complexity: Small_

- [ ] 5\. Clamp offset on resize
  - Extend `WindowSizeMsg` handling to clamp each list `offset` to `[0, max(0, rowCount−pageSize)]`, alongside the existing `detail.offset` clamp.
  - Acceptance Criteria:
    - After a resize no list offset points past the last full window.
  - _Dependencies: 1_
  - _Requirements: CR-007_
  - _Complexity: Small_

- [ ] 6\. Clamp the shared cursor to the filtered row count
  - Change the `m.cursor` clamp (tui.go 232–233) to clamp against the visible (filtered) row count.
  - Acceptance Criteria:
    - `m.cursor` never exceeds the number of currently visible rows.
  - _Dependencies: 1_
  - _Requirements: CR-008_
  - _Complexity: Small_

- [ ] 7\. Render the clipped-rows affordance
  - Show a "… N more" indicator when visible rows exceed `offset+pageSize`, matching the detail-overlay style (tui.go 1291).
  - Acceptance Criteria:
    - The indicator shows the exact count of rows below the window and disappears when none remain.
  - _Dependencies: 2_
  - _Requirements: AR-009_
  - _Complexity: Small_

- [ ] 8\. Tests — list scrolling and pane-height invariant
  - Add scroll/clamp tests paralleling `TestDetailViewScrolls`, plus a "renders within pane height" assertion per list tab paralleling tui_test.go 267–268.
  - Acceptance Criteria:
    - Tests cover scroll down/up, resize clamp, and that no list tab renders more rows than the pane height.
  - _Dependencies: 2, 3, 5, 7_
  - _Requirements: AR-010_
  - _Complexity: Medium_

## Workstream B — In-list search / filter

- [ ] 9\. Add search state and bind `/`
  - Add per-tab query + search-input-mode flag to `Model`; bind the currently-unbound `/` to enter search on Agents/Escalations/Audit/Rules.
  - Acceptance Criteria:
    - Pressing `/` on a list tab enters search-input mode; no existing binding changes.
  - _Dependencies: 1_
  - _Requirements: AR-011_
  - _Complexity: Medium_

- [ ] 10\. Incremental query editing
  - Append typed characters, backspace removes the last, recompute filtered rows after each edit.
  - Acceptance Criteria:
    - The filtered list updates on every keystroke while in search mode.
  - _Dependencies: 9_
  - _Requirements: AR-012_
  - _Complexity: Small_

- [ ] 11\. Case-insensitive substring filter
  - Filter each tab's rows by case-insensitive substring over the visible column text.
  - Acceptance Criteria:
    - Only rows containing the query (any case) in a visible column are shown.
  - _Dependencies: 10_
  - _Requirements: AR-013_
  - _Complexity: Medium_

- [ ] 12\. Exit and clear behavior
  - `esc` or `enter` with a non-empty query exits search-input mode retaining the filter; with an empty query exits with no filter; `enter` must not trigger the tab's enter action (confirm/detail/edit); backspace-to-empty restores the full list.
  - Acceptance Criteria:
    - Exit-retains-filter (via esc and enter), exit-empty, and clear-via-backspace all behave as specified; enter in search mode never confirms/opens.
  - _Dependencies: 10_
  - _Requirements: AR-014, AR-015_
  - _Complexity: Small_

- [ ] 13\. Recompute cursor and offset on query change
  - On any query change, clamp shared `m.cursor` and the tab's `offset` to the new filtered row count.
  - Acceptance Criteria:
    - Cursor and offset stay in bounds as the result set shrinks/grows.
  - _Dependencies: 6, 11_
  - _Requirements: CR-016_
  - _Complexity: Small_

- [ ] 14\. Compose search with the Rules mode filter
  - Apply the new search together with the existing Rules `f`-mode cycle (`visibleSignatures()`, tui.go 124–136, 405).
  - Acceptance Criteria:
    - On Rules, mode filter and search query both constrain the list simultaneously.
  - _Dependencies: 11_
  - _Requirements: CR-017_
  - _Complexity: Small_

- [ ] 15\. Empty-result feedback
  - Render an explicit "no rows match" empty-state per tab, consistent with tui.go 1256.
  - Acceptance Criteria:
    - A zero-match filter shows the empty-state message instead of a blank body.
  - _Dependencies: 11_
  - _Requirements: AR-018_
  - _Complexity: Small_

- [ ] 16\. Suppress action bindings while typing
  - While in search-input mode, route ALL printable keys to the query and do not invoke action or navigation bindings — in particular `q` (quit), `y` (confirm+send), `p r e c n v f a t x X`, `j`/`k` (cursor), `h`/`l` (tab switch), and space.
  - Acceptance Criteria:
    - Typing a query never triggers a destructive or navigational action — including a query containing `q` (must not quit) or `y` (must not confirm+send).
  - _Dependencies: 9_
  - _Requirements: CR-019_
  - _Complexity: Medium_

- [ ] 17\. Tests — search behavior
  - Cover per-tab search filtering (paralleling the Rules `f`-filter test), Rules search+mode composition, and the "typing does not fire actions" guard.
  - Acceptance Criteria:
    - Tests assert filtering, composition, and action suppression.
  - _Dependencies: 11, 14, 16_
  - _Requirements: AR-013, CR-017, CR-019_
  - _Complexity: Medium_

## Workstream C — Theming & palette

- [ ] 18\. Introduce a Palette type and refactor style vars
  - Define a `Palette` with roles (title, section, error, ok, paused, running, help) and refactor the package-level style vars (tui.go 41–50) to resolve from a palette instance.
  - Acceptance Criteria:
    - No style is built from a scattered hardcoded `lipgloss.Color` literal; all resolve through the palette.
  - _Dependencies: none_
  - _Requirements: CR-020_
  - _Complexity: Large_

- [ ] 19\. Define the default palette from current colors
  - Populate the `default` palette with the exact current 256-color values so it is byte-identical to today's look.
  - Acceptance Criteria:
    - Rendering with the default palette is visually identical to the pre-change TUI.
  - _Dependencies: 18_
  - _Requirements: AR-022_
  - _Complexity: Small_

- [ ] 20\. Add named themes and the selector
  - Add `dark`, `light`, `high-contrast` palettes and a selector keyed on `[tui] theme`.
  - Acceptance Criteria:
    - Each named theme is selectable and renders its distinct palette.
  - _Dependencies: 19, 25_
  - _Requirements: AR-021_
  - _Complexity: Medium_

- [ ] 21\. Resolve empty/default/unknown consistently
  - Resolve empty/absent `theme` and `theme = "default"` to the identical default palette; resolve an unknown name to the default.
  - Acceptance Criteria:
    - `""`, `"default"`, and any unknown name all yield the default palette.
  - _Dependencies: 20_
  - _Requirements: AR-022, AR-030_
  - _Complexity: Small_

- [ ] 22\. Apply per-role overrides
  - Layer optional per-role overrides on top of the selected named theme; unset roles inherit the theme value.
  - Acceptance Criteria:
    - An override changes only its role; other roles keep the theme's values.
  - _Dependencies: 20, 25_
  - _Requirements: AR-023_
  - _Complexity: Medium_

- [ ] 23\. Route empty/loading/error states through the palette
  - Style empty-list, loading (pre-first-data), and error states via the palette, including the `errStyle.Render("error: …")` path (tui.go 1192).
  - Acceptance Criteria:
    - All three states render with palette-sourced styles.
  - _Dependencies: 18_
  - _Requirements: CR-024_
  - _Complexity: Small_

- [ ] 24\. Tests — theme resolution and overrides
  - Cover default==empty==unknown-fallback, each named theme selectable, and per-role override layering.
  - Acceptance Criteria:
    - Tests assert resolution equivalence, selection, and override inheritance.
  - _Dependencies: 21, 22_
  - _Requirements: AR-021, AR-022, AR-023, AR-030_
  - _Complexity: Medium_

## Workstream D — Config plumbing

- [ ] 25\. Add TUI theme config fields
  - Add optional `Theme string` and a per-role override field to `config.TUI` with TOML tags; leave `MaxContentWidth` untouched.
  - Acceptance Criteria:
    - New fields parse from `[tui]` and are ignored when absent.
  - _Dependencies: none_
  - _Requirements: AR-027, AR-028_
  - _Complexity: Small_

- [ ] 26\. Verify backward-compatible defaults
  - Confirm `config.Default()` and existing-file loading treat the new fields as optional; add a test proving an existing `[tui]` file (only `MaxContentWidth`) still loads and renders unchanged.
  - Acceptance Criteria:
    - A legacy `[tui]` config loads without error and yields the default palette.
  - _Dependencies: 25_
  - _Requirements: CR-029_
  - _Complexity: Small_

## Workstream E — Status area & non-impact verification

- [ ] 27\. Rework the status area to be durable
  - Rework `m.message` rendering into a durable, clearly-placed status area that persists an outcome long enough to read after multi-step operations.
  - Acceptance Criteria:
    - An action outcome remains readable in a fixed status area after the operation completes.
  - _Dependencies: none_
  - _Requirements: CR-025_
  - _Complexity: Large_

- [ ] 28\. Source ok/error status styles from the palette
  - Preserve success-vs-error differentiation by sourcing the ok/error styles from the palette (was tui.go 251–253).
  - Acceptance Criteria:
    - Success and error outcomes remain visually distinct, now palette-sourced.
  - _Dependencies: 18, 27_
  - _Requirements: CR-026_
  - _Complexity: Small_

- [ ] 29\. Verify no frontend API change
  - Confirm no `frontend.App` method signature or returned shape changed; the TUI still reads via existing `App` methods (`GetStatus`, `Escalations`, `Audit`, `Signatures`, …).
  - Acceptance Criteria:
    - `internal/frontend` has no signature diff; TUI data paths unchanged.
  - _Dependencies: none_
  - _Requirements: AR-031_
  - _Complexity: Small_

## Workstream F — Rules full signature id (new)

- [ ] 30\. Show the full signature id in the Rules list
  - In `renderSignatures` (tui.go 1249–), replace the truncated `shortSig(r.Signature)` column (tui.go 1267; `shortSig` at 1038–1043) with the full `r.Signature`, and widen/reflow the row column budget (the ~66-cell fixed prefix noted at tui.go 1263) so a full id like `idle:83b2285833fd4c6ebf40a2bc` fits without clipping the trailing columns. Preserve graceful narrow-terminal behavior via the existing width budgeting.
  - Acceptance Criteria:
    - The Rules list renders each signature id in full (e.g. `idle:83b2285833fd4c6ebf40a2bc`), not `idle:83b2285833f…`.
    - Other Rules columns still fit within `contentWidth()` and narrow terminals do not clip the id mid-string without an intentional affordance.
    - The signature-detail overlay (tui.go 996) is unchanged.
  - _Dependencies: 2_
  - _Requirements: CR-032_
  - _Complexity: Medium_

## Workstream G — Config tab completeness (new)

- [ ] 31\. Expose the missing scalar config fields
  - Add `llm.pane_excerpt_chars` (config.go 83), `safety.disable_seed` (config.go 44), and `tui.max_content_width` (config.go 162) — plus the new `tui.theme` once task 25 lands — to `ConfigFieldKeys` (frontend.go 450–471), `FieldValue` (474–521), and `SetField` (525–) with type-appropriate validation, so the Config tab and `config set` both display and edit them (CLI parity, FR-022).
  - Acceptance Criteria:
    - Each field appears on the Config tab with its current value and is editable via enter/`e` and `config set`.
    - Invalid values are rejected with a clear message; no existing key changes behavior.
  - _Dependencies: 25_
  - _Requirements: CR-033_
  - _Complexity: Medium_

- [ ] 32\. Render read-only Safety-indicator and capture-delay sections
  - Extend `buildRuleItems` (tui.go 184–217) with read-only rows: a "Safety indicators" section for `safety.irreversible_indicators` and each `[[safety.indicator_rules]]` entry (pattern + agent scope, config.go 48–59), and a capture-delay section showing each `[[capture_delay]]` rule (config.go 151–155) or the built-in defaults (10000/2000 ms) when none are configured. Edit/remove keys on these rows show an informational "edited in config.toml" message (mirroring tui.go 1140) and never mutate config. Classifier manifests stay file-only.
  - Acceptance Criteria:
    - Configured indicator patterns, indicator rules, and capture-delay rules are visible on the Config tab; the defaults row appears when no capture-delay rule exists.
    - `enter`/`e`/`x` on the read-only rows produce the informational message and change nothing.
  - _Dependencies: none_
  - _Requirements: AR-034, AR-035_
  - _Complexity: Medium_

- [ ] 33\. Classify field editability and truncate long values
  - Add a TUI-editable / TUI-read-only classification over the field registry, stored in one place (per-key editable flag or type tag) so new fields default read-only: numeric/boolean/short-enum fields stay editable via the inline prompt; ANY free-text field — argv templates, template strings, paths, other long strings (today: `llm.command`, `llm.rewrite_command`, `llm.rewrite_fallback_template`, `embedding.model_path`; plus per-role palette overrides if displayed) — is TUI-read-only — `enter`/`e` on them (`editSelectedRule`, tui.go 1054–1074) shows an informational "edit in config.toml (or `config set`)" message and never opens the prompt. `SetField` keeps accepting all keys (CLI parity). Truncate each config row's value to the row budget with an ellipsis (via `oneLine`, tui.go 1452) so long commands no longer overflow the row (`buildRuleItems` label, tui.go 189).
  - Acceptance Criteria:
    - Read-only fields show the informational message on `enter`/`e` and cannot be edited from the TUI; `config set llm.command …` still works.
    - Every editable field still opens the prompt and saves; long values render on one line with a trailing ellipsis.
  - _Dependencies: 31_
  - _Requirements: CR-036, CR-037_
  - _Complexity: Medium_

- [ ] 34\. Tests — config completeness, editability, and registry parity
  - Add a three-way parity test failing when a key is present in one of `ConfigFieldKeys`/`FieldValue`/`SetField` but missing in another (extending the iteration test at frontend_test.go 379); cover display + edit of each newly exposed field, the read-only field classification (message shown, no prompt, no mutation; `SetField` still accepts the key), value truncation, and rendering of the read-only sections (including the defaults row and the no-mutation guarantee).
  - Acceptance Criteria:
    - Parity test fails on a deliberately removed case; new-field edit, read-only classification, truncation, and section render tests pass.
  - _Dependencies: 31, 32, 33_
  - _Requirements: CR-033, AR-034, AR-035, CR-036, CR-037_
  - _Complexity: Medium_

## Finalization

- [ ] 35\. Update docs
  - Update README / config reference for the new `[tui] theme` + per-role override keys, the `/` search keybinding, the full Rules signature-id display, the newly exposed / newly displayed config fields (`llm.pane_excerpt_chars`, `safety.disable_seed`, `tui.max_content_width`, safety indicators, capture delays), and which fields are TUI-read-only (edit via `config.toml` / `config set`).
  - Acceptance Criteria:
    - Docs describe the new config keys, search key, Rules id change, and Config tab completeness/editability changes.
  - _Dependencies: 25, 30, 31, 32, 33_
  - _Requirements: AR-027, AR-028, AR-021, CR-032, CR-033, CR-036_
  - _Complexity: Small_

- [ ] 36\. Full test + smoke pass
  - Run `go test ./internal/tui/... ./internal/config/... ./internal/frontend/...` and confirm the suite passes; manually smoke-test long lists, search, each theme, the status area, the full Rules id, and the completed Config tab (new fields, read-only rows, truncation) in a live pane.
  - Acceptance Criteria:
    - All tests pass; manual smoke test confirms every workstream in a live pane.
  - _Dependencies: 8, 17, 24, 26, 28, 29, 30, 34_
  - _Requirements: AR-010, CR-017, AR-023, CR-029, CR-026, AR-031, CR-032, CR-033, AR-034, AR-035, CR-036, CR-037_
  - _Complexity: Medium_
